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
their own pace; nothing is force-suspended), deletes the emptied old-version
worker pods, flips the node's ` + versionlabel.Key + ` label, and waits for
the node's atelet and target-version worker pods to be Ready.

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
	fmt.Fprintf(r.out, "Done: %d node(s) at version %q\n", len(nodes), target)
	return nil
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

	if err := r.waitNodeEmpty(ctx, node.Name); err != nil {
		return err
	}

	// Re-list: pods may have terminated while we waited.
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

	if current != target {
		fmt.Fprintf(r.out, "  labeling node %s %s=%s\n", node.Name, versionlabel.Key, target)
		if err := r.kube.PatchNodeLabel(ctx, node.Name, versionlabel.Key, target); err != nil {
			return err
		}
	}

	return r.waitNodeReady(ctx, node.Name, target)
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
// Purely passive: actors leave at the user's pace.
func (r *UpgradeRollRunner) waitNodeEmpty(ctx context.Context, node string) error {
	var deadline time.Time
	if r.drainTimeout > 0 {
		deadline = time.Now().Add(r.drainTimeout)
	}
	for {
		actors, err := listAllActors(ctx, r.api)
		if err != nil {
			return err
		}
		workers, err := listAllWorkers(ctx, r.api)
		if err != nil {
			return err
		}
		blocking := blockingActorsOnNode(actors, workers, node)
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

// waitNodeReady polls until the node's atelet pod and the target-version
// worker pods scheduled to the node are Ready. If no target-version worker pod
// has been scheduled to the node by the deadline (e.g. the pools' replicas are
// seated on other nodes), it warns and proceeds; pods that are present but not
// Ready at the deadline are an error.
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

		if ateletOK && len(targetPods) > 0 && ready == len(targetPods) {
			fmt.Fprintf(r.out, "  node %s ready: atelet Ready, %d/%d worker pod(s) Ready at %s\n", node, ready, len(targetPods), target)
			return nil
		}
		if time.Now().After(deadline) {
			if ateletOK && len(targetPods) == 0 {
				fmt.Fprintf(r.out, "  WARNING: no %s worker pods scheduled to node %s after %s (pools may be seated elsewhere); continuing\n", target, node, r.readyTimeout)
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
// version. Versioned atelet DaemonSets label their pods; an unlabeled pod
// belongs to a pre-versioning install and counts too.
func ateletReady(pods []corev1.Pod, target string) bool {
	for i := range pods {
		v := podVersion(&pods[i])
		if (v == "" || v == target) && isPodReady(&pods[i]) {
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
