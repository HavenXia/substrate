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
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/agent-substrate/substrate/internal/versionlabel"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	// workerPoolLabelKey marks worker pods and their Deployments; stamped by
	// atecontroller (see cmd/atecontroller/internal/controllers/workerpool_apply.go).
	workerPoolLabelKey = "ate.dev/worker-pool"

	ateSystemNamespace = "ate-system"
	ateletPodSelector  = "app=atelet"
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Roll substrate dataplane versions across nodes",
}

func init() {
	rootCmd.AddCommand(upgradeCmd)
}

// upgradeAPI is the slice of the ate-api Control service used by the upgrade
// driver. *ateclient.Client satisfies it.
type upgradeAPI interface {
	WorkerLister
	ListActors(ctx context.Context, in *ateapipb.ListActorsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorsResponse, error)
	DrainWorker(ctx context.Context, in *ateapipb.DrainWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error)
}

// upgradeKube is the narrow Kubernetes surface the upgrade driver needs.
// Version filtering happens in the driver, from pod labels, so mocks only
// serve fixtures.
type upgradeKube interface {
	ListNodes(ctx context.Context) ([]corev1.Node, error)
	// PatchNodeLabel sets one label on a node, leaving other labels alone.
	PatchNodeLabel(ctx context.Context, nodeName, key, value string) error
	// ListWorkerPods returns pods carrying the worker-pool label across all
	// namespaces, restricted to one node when node is non-empty.
	ListWorkerPods(ctx context.Context, node string) ([]corev1.Pod, error)
	// ListAteletPods returns atelet pods, restricted to one node when node is
	// non-empty.
	ListAteletPods(ctx context.Context, node string) ([]corev1.Pod, error)
	DeletePod(ctx context.Context, namespace, name string) error
	// ListWorkerDeployments returns Deployments carrying the worker-pool label
	// across all namespaces.
	ListWorkerDeployments(ctx context.Context) ([]appsv1.Deployment, error)
	DeleteDeployment(ctx context.Context, namespace, name string) error
	// ListAteletDaemonSets returns the atelet DaemonSets, labeled and
	// unlabeled alike.
	ListAteletDaemonSets(ctx context.Context) ([]appsv1.DaemonSet, error)
	DeleteDaemonSet(ctx context.Context, namespace, name string) error
}

// kubeUpgradeClient implements upgradeKube on a Kubernetes clientset.
type kubeUpgradeClient struct {
	clientset kubernetes.Interface
}

func (c *kubeUpgradeClient) ListNodes(ctx context.Context) ([]corev1.Node, error) {
	list, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	return list.Items, nil
}

func (c *kubeUpgradeClient) PatchNodeLabel(ctx context.Context, nodeName, key, value string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{key: value},
		},
	})
	if err != nil {
		return err
	}
	_, err = c.clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
	}
	return nil
}

func (c *kubeUpgradeClient) ListWorkerPods(ctx context.Context, node string) ([]corev1.Pod, error) {
	opts := metav1.ListOptions{LabelSelector: workerPoolLabelKey}
	if node != "" {
		opts.FieldSelector = "spec.nodeName=" + node
	}
	list, err := c.clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to list worker pods: %w", err)
	}
	return list.Items, nil
}

func (c *kubeUpgradeClient) ListAteletPods(ctx context.Context, node string) ([]corev1.Pod, error) {
	opts := metav1.ListOptions{LabelSelector: ateletPodSelector}
	if node != "" {
		opts.FieldSelector = "spec.nodeName=" + node
	}
	list, err := c.clientset.CoreV1().Pods(ateSystemNamespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to list atelet pods: %w", err)
	}
	return list.Items, nil
}

func (c *kubeUpgradeClient) DeletePod(ctx context.Context, namespace, name string) error {
	return c.clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *kubeUpgradeClient) ListWorkerDeployments(ctx context.Context) ([]appsv1.Deployment, error) {
	list, err := c.clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: workerPoolLabelKey})
	if err != nil {
		return nil, fmt.Errorf("failed to list worker deployments: %w", err)
	}
	return list.Items, nil
}

func (c *kubeUpgradeClient) DeleteDeployment(ctx context.Context, namespace, name string) error {
	return c.clientset.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *kubeUpgradeClient) ListAteletDaemonSets(ctx context.Context) ([]appsv1.DaemonSet, error) {
	list, err := c.clientset.AppsV1().DaemonSets(ateSystemNamespace).List(ctx, metav1.ListOptions{LabelSelector: ateletPodSelector})
	if err != nil {
		return nil, fmt.Errorf("failed to list atelet daemonsets: %w", err)
	}
	return list.Items, nil
}

func (c *kubeUpgradeClient) DeleteDaemonSet(ctx context.Context, namespace, name string) error {
	return c.clientset.AppsV1().DaemonSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// listAllActors pages through ListActors across all atespaces.
func listAllActors(ctx context.Context, api upgradeAPI) ([]*ateapipb.Actor, error) {
	var actors []*ateapipb.Actor
	pageToken := ""
	for {
		resp, err := api.ListActors(ctx, &ateapipb.ListActorsRequest{
			PageSize:  1000,
			PageToken: pageToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list actors: %w", err)
		}
		actors = append(actors, resp.GetActors()...)
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}
	return actors, nil
}

// podVersion returns a pod's substrate version label value, empty if unlabeled.
func podVersion(pod *corev1.Pod) string {
	return pod.Labels[versionlabel.Key]
}

// nodeVersion returns a node's substrate version label value, empty if unlabeled.
func nodeVersion(node *corev1.Node) string {
	return node.Labels[versionlabel.Key]
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podsNotAtVersion returns the pods whose version label differs from target
// (including unlabeled pods).
func podsNotAtVersion(pods []corev1.Pod, target string) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		if podVersion(&pods[i]) != target {
			out = append(out, pods[i])
		}
	}
	return out
}

// podsAtVersion returns the pods whose version label equals target.
func podsAtVersion(pods []corev1.Pod, target string) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		if podVersion(&pods[i]) == target {
			out = append(out, pods[i])
		}
	}
	return out
}

// blockingActorsOnNode returns human-readable descriptions of the actors that
// keep a node from being drained: actors bound to a worker on the node
// (RUNNING and the transitional states carry a worker_assignment), and PAUSED
// actors whose node-local snapshot lives on the node. SUSPENDED and CRASHED
// actors hold no node state and never block.
//
// Actor->node binding is resolved by joining actor
// status.worker_assignment.worker.name against Worker.metadata.name from
// ListWorkers (WorkerAssignment itself has no node_name). Workers on the node
// that report a bound actor the actor list missed (pagination races) block too.
func blockingActorsOnNode(actors []*ateapipb.Actor, workers []*ateapipb.Worker, node string) []string {
	nodeWorkers := make(map[string]*ateapipb.Worker)
	for _, w := range workers {
		if w.GetNodeName() == node {
			nodeWorkers[w.GetMetadata().GetName()] = w
		}
	}

	blocking := make(map[string]string) // atespace/name -> description
	actorKey := func(ref *ateapipb.ObjectRef) string {
		return ref.GetAtespace() + "/" + ref.GetName()
	}

	for _, a := range actors {
		key := actorKey(&ateapipb.ObjectRef{Atespace: a.GetMetadata().GetAtespace(), Name: a.GetMetadata().GetName()})
		state := a.GetStatus().GetState()
		if wa := a.GetStatus().GetWorkerAssignment(); wa != nil {
			if w, ok := nodeWorkers[wa.GetWorker().GetName()]; ok {
				blocking[key] = fmt.Sprintf("%s (%s on pod %s/%s)", key, actorStateShort(state), w.GetWorkerNamespace(), w.GetWorkerPod())
				continue
			}
		}
		if state == ateapipb.ActorState_ACTOR_STATE_PAUSED &&
			slices.Contains(a.GetStatus().GetLocalSnapshotInfo().GetNodeVmsWithLocalSnapshots(), node) {
			blocking[key] = fmt.Sprintf("%s (PAUSED, local snapshot on node)", key)
		}
	}

	for _, w := range nodeWorkers {
		assignment := w.GetStatus().GetAssignment()
		if assignment == nil {
			continue
		}
		key := actorKey(assignment.GetActor())
		if _, ok := blocking[key]; !ok {
			blocking[key] = fmt.Sprintf("%s (bound to worker pod %s/%s)", key, w.GetWorkerNamespace(), w.GetWorkerPod())
		}
	}

	var out []string
	for _, desc := range blocking {
		out = append(out, desc)
	}
	sort.Strings(out)
	return out
}

func actorStateShort(state ateapipb.ActorState) string {
	const prefix = "ACTOR_STATE_"
	s := state.String()
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// substrateNodes returns the names of nodes that are part of the substrate
// dataplane: labeled with a substrate version, or hosting worker or atelet
// pods. Sorted ascending.
func substrateNodes(nodes []corev1.Node, workerPods, ateletPods []corev1.Pod) []string {
	eligible := make(map[string]bool)
	for i := range nodes {
		if _, ok := nodes[i].Labels[versionlabel.Key]; ok {
			eligible[nodes[i].Name] = true
		}
	}
	known := make(map[string]bool, len(nodes))
	for i := range nodes {
		known[nodes[i].Name] = true
	}
	for _, pods := range [][]corev1.Pod{workerPods, ateletPods} {
		for i := range pods {
			if n := pods[i].Spec.NodeName; n != "" && known[n] {
				eligible[n] = true
			}
		}
	}
	names := make([]string, 0, len(eligible))
	for n := range eligible {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
