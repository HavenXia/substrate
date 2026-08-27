// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/versionlabel"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var (
	upgradeRunTargetVersion string
	upgradeRunNodes         []string
	upgradeRunPollInterval  time.Duration
	upgradeRunDrainTimeout  time.Duration
	upgradeRunReadyTimeout  time.Duration
)

var upgradeRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Roll nodes to a target substrate version, one node at a time",
	Long: `Rolls each substrate node to the target version: drains the node's workers,
waits until no RUNNING or PAUSED actor occupies the node (actors leave at
their own pace; nothing is force-suspended), flips the node's
` + versionlabel.Key + ` label, deletes the emptied old-version worker pods,
and waits for the node's atelet and target-version worker pods to be Ready.

The command is idempotent: every step derives its state from the cluster, so
rerunning skips nodes that are already done.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUpgradeRoll(cmd, false)
	},
}

func init() {
	addUpgradeRollFlags(upgradeRunCmd)
	upgradeCmd.AddCommand(upgradeRunCmd)
}

func addUpgradeRollFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&upgradeRunTargetVersion, "target-version", "", "Substrate build version to roll nodes to")
	_ = cmd.MarkFlagRequired("target-version")
	cmd.Flags().StringArrayVar(&upgradeRunNodes, "node", nil, "Only process these nodes, in the given order (repeatable). Defaults to every substrate node.")
	cmd.Flags().DurationVar(&upgradeRunPollInterval, "poll-interval", 5*time.Second, "Interval between actor/pod polls")
	cmd.Flags().DurationVar(&upgradeRunDrainTimeout, "drain-timeout", 0, "Per-node limit on waiting for actors to leave; 0 waits forever")
	cmd.Flags().DurationVar(&upgradeRunReadyTimeout, "ready-timeout", 10*time.Minute, "Per-node limit on waiting for atelet and worker pods to be Ready")
}

func runUpgradeRoll(cmd *cobra.Command, rollback bool) error {
	ctx := cmd.Context()
	apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, tokenFile, traceEnabled)
	if err != nil {
		return fmt.Errorf("failed to connect to ate-api-server: %w", err)
	}
	defer apiClient.Close()

	k8sClient, err := ateclient.NewK8sClientset(kubeconfig, k8sContext)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	runner := &UpgradeRollRunner{
		api:           apiClient,
		kube:          &kubeUpgradeClient{clientset: k8sClient},
		targetVersion: upgradeRunTargetVersion,
		nodes:         upgradeRunNodes,
		rollback:      rollback,
		pollInterval:  upgradeRunPollInterval,
		drainTimeout:  upgradeRunDrainTimeout,
		readyTimeout:  upgradeRunReadyTimeout,
		out:           os.Stdout,
	}
	return runner.Run(ctx)
}

// UpgradeRollRunner rolls nodes to a target version one at a time. rollback
// only changes the default node order (reverse) and wording; the mechanics
// are symmetric because old-version worker Deployments keep Pending pods that
// reseat when a node's label flips back.
type UpgradeRollRunner struct {
	api           upgradeAPI
	kube          upgradeKube
	targetVersion string
	nodes         []string
	rollback      bool
	pollInterval  time.Duration
	drainTimeout  time.Duration
	readyTimeout  time.Duration
	out           io.Writer
}

func (r *UpgradeRollRunner) Run(ctx context.Context) error {
	if r.out == nil {
		r.out = os.Stdout
	}
	if r.pollInterval <= 0 {
		r.pollInterval = 5 * time.Second
	}
	if r.readyTimeout <= 0 {
		r.readyTimeout = 10 * time.Minute
	}
	if r.targetVersion == "" {
		return fmt.Errorf("--target-version is required")
	}
	target := versionlabel.Value(r.targetVersion)

	// The label flip removes a node's old atelet, so without the target
	// version's DaemonSet staged the converted node ends up ateletless. Verify
	// before touching any node.
	dss, err := r.kube.ListAteletDaemonSets(ctx)
	if err != nil {
		return err
	}
	hasTarget := false
	for i := range dss {
		if dss[i].Labels[versionlabel.Key] == target {
			hasTarget = true
		}
	}
	if !hasTarget {
		return fmt.Errorf("no atelet DaemonSet labeled %s=%s: apply the target release's manifests first, then rerun", versionlabel.Key, target)
	}

	nodes, err := r.selectNodes(ctx)
	if err != nil {
		return err
	}

	verb := "Rolling"
	if r.rollback {
		verb = "Rolling back"
	}
	fmt.Fprintf(r.out, "%s %d node(s) to version %q (label %s=%s)\n", verb, len(nodes), r.targetVersion, versionlabel.Key, target)

	for i, node := range nodes {
		if err := r.rollNode(ctx, i+1, len(nodes), node, target); err != nil {
			return fmt.Errorf("node %s: %w", node.Name, err)
		}
	}

	// Nodes can join or revert while the pass above runs: the autoscaler adds
	// nodes born with the old pool label, and a node recreation (auto-repair,
	// auto-upgrade) comes back at the pool label too. Re-list and keep rolling
	// until a pass finds nothing left, so one invocation converges. Explicit
	// --node lists stay a single pass: the user picked the exact set.
	if len(r.nodes) == 0 {
		for round := 2; ; round++ {
			pending, err := r.pendingNodes(ctx, target)
			if err != nil {
				return err
			}
			if len(pending) == 0 {
				break
			}
			if round > maxRollRounds {
				return fmt.Errorf("nodes still need converting after %d passes (is something still producing old-version nodes, e.g. an uncapped autoscaler?): %s", maxRollRounds, nodeNames(pending))
			}
			fmt.Fprintf(r.out, "Pass %d: %d node(s) joined or reverted during the roll\n", round, len(pending))
			for i, node := range pending {
				if err := r.rollNode(ctx, i+1, len(pending), node, target); err != nil {
					return fmt.Errorf("node %s: %w", node.Name, err)
				}
			}
		}
	}
	fmt.Fprintf(r.out, "Done: all substrate nodes at version %q\n", target)
	return nil
}

// maxRollRounds bounds the converge loop. Passes beyond the first only happen
// when nodes join or revert mid-roll; needing this many means something keeps
// producing old-version nodes faster than the roll converts them.
const maxRollRounds = 10

// pendingNodes returns the substrate nodes that still need work: wrong version
// label or worker pods not at the target version, in selectNodes order.
// Terminating pods do not count: right after a pass, the just-deleted old
// pods linger in the API for their (long) grace period, and counting them
// would spin the converge loop over already-done nodes until maxRollRounds.
func (r *UpgradeRollRunner) pendingNodes(ctx context.Context, target string) ([]corev1.Node, error) {
	nodes, err := r.selectNodes(ctx)
	if err != nil {
		return nil, err
	}
	var pending []corev1.Node
	for _, node := range nodes {
		if nodeVersion(&node) != target {
			pending = append(pending, node)
			continue
		}
		pods, err := r.kube.ListWorkerPods(ctx, node.Name)
		if err != nil {
			return nil, err
		}
		for _, pod := range podsNotAtVersion(pods, target) {
			if pod.DeletionTimestamp == nil {
				pending = append(pending, node)
				break
			}
		}
	}
	return pending, nil
}

func nodeNames(nodes []corev1.Node) string {
	names := make([]string, 0, len(nodes))
	for i := range nodes {
		names = append(names, nodes[i].Name)
	}
	return summarizeList(names, 5)
}

// selectNodes returns the nodes to process. Explicit --node names are used in
// the given order; otherwise every substrate node, sorted ascending (or
// descending for rollback).
func (r *UpgradeRollRunner) selectNodes(ctx context.Context) ([]corev1.Node, error) {
	nodes, err := r.kube.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]corev1.Node, len(nodes))
	for _, n := range nodes {
		byName[n.Name] = n
	}

	if len(r.nodes) > 0 {
		out := make([]corev1.Node, 0, len(r.nodes))
		for _, name := range r.nodes {
			n, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("node %q not found", name)
			}
			out = append(out, n)
		}
		return out, nil
	}

	workerPods, err := r.kube.ListWorkerPods(ctx, "")
	if err != nil {
		return nil, err
	}
	ateletPods, err := r.kube.ListAteletPods(ctx, "")
	if err != nil {
		return nil, err
	}
	names := substrateNodes(nodes, workerPods, ateletPods)
	if len(names) == 0 {
		return nil, fmt.Errorf("no substrate nodes found (no %s label, worker pods, or atelet pods); use --node to name nodes explicitly", versionlabel.Key)
	}
	if r.rollback {
		for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
			names[i], names[j] = names[j], names[i]
		}
	}
	out := make([]corev1.Node, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out, nil
}

func (r *UpgradeRollRunner) rollNode(ctx context.Context, i, n int, node corev1.Node, target string) error {
	current := nodeVersion(&node)
	currentDisplay := current
	if currentDisplay == "" {
		currentDisplay = "<none>"
	}
	fmt.Fprintf(r.out, "[%d/%d] node %s (version %s)\n", i, n, node.Name, currentDisplay)

	pods, err := r.kube.ListWorkerPods(ctx, node.Name)
	if err != nil {
		return err
	}
	oldPods := podsNotAtVersion(pods, target)

	if current == target && len(oldPods) == 0 {
		fmt.Fprintf(r.out, "  already at %s with no other-version worker pods; verifying readiness\n", target)
		return r.waitNodeReady(ctx, node.Name, target)
	}

	if len(oldPods) > 0 {
		if err := r.drainNodeWorkers(ctx, node.Name, oldPods); err != nil {
			return err
		}
	}

	if err := r.waitNodeEmpty(ctx, node.Name, target); err != nil {
		return err
	}

	// Flip the label before deleting: the old set's ReplicaSet replaces every
	// deleted pod immediately, and on a still-old-labeled node the scheduler
	// could seat that undrained replacement right back here. Flipped first,
	// replacements stay Pending (the rollback spring).
	if current != target {
		fmt.Fprintf(r.out, "  labeling node %s %s=%s\n", node.Name, versionlabel.Key, target)
		if err := r.kube.PatchNodeLabel(ctx, node.Name, versionlabel.Key, target); err != nil {
			// Nodes vanish mid-roll (autoscaler scale-down, pool repair); the
			// list the pass iterates is a snapshot. A gone node needs nothing
			// from us, and its replacement, if any, is picked up by the next
			// converge pass.
			if apierrors.IsNotFound(err) {
				fmt.Fprintf(r.out, "  node %s no longer exists; skipping\n", node.Name)
				return nil
			}
			return err
		}
	}

	// The delete is still needed after the flip: a label change never evicts
	// running pods (node affinity is scheduling-time only). Re-list: pods may
	// have terminated while we waited.
	pods, err = r.kube.ListWorkerPods(ctx, node.Name)
	if err != nil {
		return err
	}
	for _, pod := range podsNotAtVersion(pods, target) {
		fmt.Fprintf(r.out, "  deleting worker pod %s/%s (version %q)\n", pod.Namespace, pod.Name, podVersion(&pod))
		if err := r.kube.DeletePod(ctx, pod.Namespace, pod.Name); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}

	return r.waitNodeReady(ctx, node.Name, target)
}

// unscheduledTargetPodsExist reports whether any target-version worker pod is
// still waiting for a node.
func (r *UpgradeRollRunner) unscheduledTargetPodsExist(ctx context.Context, target string) (bool, error) {
	pods, err := r.kube.ListWorkerPods(ctx, "")
	if err != nil {
		return false, err
	}
	for _, pod := range podsAtVersion(pods, target) {
		if pod.Spec.NodeName == "" && pod.DeletionTimestamp == nil {
			return true, nil
		}
	}
	return false, nil
}

// drainNodeWorkers marks the workers backing the given pods as draining so
// the scheduler stops routing new actors to them.
func (r *UpgradeRollRunner) drainNodeWorkers(ctx context.Context, node string, oldPods []corev1.Pod) error {
	podKeys := make(map[string]bool, len(oldPods))
	for i := range oldPods {
		podKeys[oldPods[i].Namespace+"/"+oldPods[i].Name] = true
	}

	workers, err := listAllWorkers(ctx, r.api)
	if err != nil {
		return err
	}
	for _, w := range workers {
		if w.GetNodeName() != node || !podKeys[w.GetWorkerNamespace()+"/"+w.GetWorkerPod()] {
			continue
		}
		if w.GetStatus().GetState() == ateapipb.WorkerState_WORKER_STATE_DRAINING {
			fmt.Fprintf(r.out, "  worker %s (pod %s/%s) already draining\n", w.GetMetadata().GetName(), w.GetWorkerNamespace(), w.GetWorkerPod())
			continue
		}
		fmt.Fprintf(r.out, "  draining worker %s (pod %s/%s)\n", w.GetMetadata().GetName(), w.GetWorkerNamespace(), w.GetWorkerPod())
		if _, err := r.api.DrainWorker(ctx, &ateapipb.DrainWorkerRequest{
			Worker: &ateapipb.ObjectRef{Name: w.GetMetadata().GetName()},
		}); err != nil {
			return fmt.Errorf("failed to drain worker %s: %w", w.GetMetadata().GetName(), err)
		}
	}
	return nil
}

// waitNodeEmpty polls until no RUNNING or PAUSED actor occupies the node.
// Purely passive: actors leave at the user's pace. Worker-bound actors block
// only when their worker backs a non-target-version pod (those are the pods
// about to be deleted); actors on target-version workers keep serving and
// must not stall a rerun over a partially rolled node. PAUSED local-snapshot
// blocking stays node-scoped.
func (r *UpgradeRollRunner) waitNodeEmpty(ctx context.Context, node, target string) error {
	var deadline time.Time
	if r.drainTimeout > 0 {
		deadline = time.Now().Add(r.drainTimeout)
	}
	for {
		pods, err := r.kube.ListWorkerPods(ctx, node)
		if err != nil {
			return err
		}
		oldPodKeys := make(map[string]bool)
		for _, pod := range podsNotAtVersion(pods, target) {
			oldPodKeys[pod.Namespace+"/"+pod.Name] = true
		}
		actors, err := listAllActors(ctx, r.api)
		if err != nil {
			return err
		}
		workers, err := listAllWorkers(ctx, r.api)
		if err != nil {
			return err
		}
		blocking := blockingActorsOnNode(actors, workers, node, oldPodKeys)
		if len(blocking) == 0 {
			fmt.Fprintf(r.out, "  node %s has no running or paused actors\n", node)
			return nil
		}
		fmt.Fprintf(r.out, "  waiting for %d actor(s) to leave node %s: %s\n", len(blocking), node, summarizeList(blocking, 5))
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("still %d actor(s) on node after %s; rerun to keep waiting", len(blocking), r.drainTimeout)
		}
		if err := sleepCtx(ctx, r.pollInterval); err != nil {
			return err
		}
	}
}

// waitNodeReady polls until the node's atelet pod is Ready, the
// target-version worker pods scheduled to the node are Ready, and no
// target-version pod is still queued unscheduled cluster-wide. The roll owes
// capacity, not this node's capacity: replacements for displaced workers may
// legitimately seat on earlier-converted nodes, and then this node is done
// the moment its atelet is — waiting a full timeout here for pods that
// already live elsewhere would only stall the roll. The cluster-wide queue
// check is what keeps a rollback over a damaged fleet honest: emptied nodes
// have nothing local to wait for, but the old version's Pending pods are
// exactly the capacity the rollback exists to restore. Absences resolve at
// the deadline as warn-and-continue — queued pods that never seat (capacity
// shortfall) or no atelet pod at all (the DaemonSet may be unable to
// schedule to this node, e.g. taints) — but pods that are present and not
// Ready are an error.
func (r *UpgradeRollRunner) waitNodeReady(ctx context.Context, node, target string) error {
	deadline := time.Now().Add(r.readyTimeout)
	for {
		ateletPods, err := r.kube.ListAteletPods(ctx, node)
		if err != nil {
			return err
		}
		ateletOK := ateletReady(ateletPods, target)

		workerPods, err := r.kube.ListWorkerPods(ctx, node)
		if err != nil {
			return err
		}
		targetPods := podsAtVersion(workerPods, target)
		ready := 0
		for i := range targetPods {
			if isPodReady(&targetPods[i]) {
				ready++
			}
		}

		if ateletOK && ready == len(targetPods) {
			queued, err := r.unscheduledTargetPodsExist(ctx, target)
			if err != nil {
				return err
			}
			if !queued {
				if len(targetPods) > 0 {
					fmt.Fprintf(r.out, "  node %s ready: atelet Ready, %d/%d worker pod(s) Ready at %s\n", node, ready, len(targetPods), target)
				} else {
					fmt.Fprintf(r.out, "  node %s ready: atelet Ready; all %s capacity is seated elsewhere\n", node, target)
				}
				return nil
			}
		}
		if time.Now().After(deadline) {
			if len(ateletPods) == 0 {
				fmt.Fprintf(r.out, "  WARNING: no atelet pod on node %s after %s (the atelet DaemonSet may be unable to schedule here); continuing\n", node, r.readyTimeout)
				ateletOK = true
			}
			if ateletOK && ready == len(targetPods) {
				fmt.Fprintf(r.out, "  WARNING: %s worker pods are still unscheduled after %s (not enough labeled capacity yet?); continuing\n", target, r.readyTimeout)
				return nil
			}
			return fmt.Errorf("not ready after %s: atelet ready=%t, %d/%d worker pod(s) ready; rerun to keep waiting", r.readyTimeout, ateletOK, ready, len(targetPods))
		}
		fmt.Fprintf(r.out, "  waiting for readiness on node %s: atelet ready=%t, %d/%d worker pod(s) ready\n", node, ateletOK, ready, len(targetPods))
		if err := sleepCtx(ctx, r.pollInterval); err != nil {
			return err
		}
	}
}

// ateletReady reports whether the node has a Ready atelet pod at the target
// version. Versioned atelet DaemonSets label their pods; unlabeled pods
// belong to pre-versioning installs, which the roll does not support
// (their DaemonSet's immutable selector cannot gain the version key, so
// those clusters take a fresh install instead).
func ateletReady(pods []corev1.Pod, target string) bool {
	for i := range pods {
		if podVersion(&pods[i]) == target && isPodReady(&pods[i]) {
			return true
		}
	}
	return false
}

func summarizeList(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(", and %d more", len(items)-max)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
