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
	"testing"

	"github.com/agent-substrate/substrate/internal/versionlabel"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockUpgradeAPI serves worker and actor fixtures. Successive ListActors
// calls step through actorsSeq (the last page repeats), which lets tests
// script a drain-poll loop.
type mockUpgradeAPI struct {
	workers   []*ateapipb.Worker
	actorsSeq [][]*ateapipb.Actor

	listActorsCalls int
	drainErr        error
	drained         []string
}

func (m *mockUpgradeAPI) ListWorkers(ctx context.Context, req *ateapipb.ListWorkersRequest, opts ...grpc.CallOption) (*ateapipb.ListWorkersResponse, error) {
	return &ateapipb.ListWorkersResponse{Workers: m.workers}, nil
}

func (m *mockUpgradeAPI) ListActors(ctx context.Context, req *ateapipb.ListActorsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	i := m.listActorsCalls
	m.listActorsCalls++
	if len(m.actorsSeq) == 0 {
		return &ateapipb.ListActorsResponse{}, nil
	}
	if i >= len(m.actorsSeq) {
		i = len(m.actorsSeq) - 1
	}
	return &ateapipb.ListActorsResponse{Actors: m.actorsSeq[i]}, nil
}

func (m *mockUpgradeAPI) DrainWorker(ctx context.Context, req *ateapipb.DrainWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error) {
	if m.drainErr != nil {
		return nil, m.drainErr
	}
	m.drained = append(m.drained, req.GetWorker().GetName())
	return &ateapipb.Worker{}, nil
}

// mockUpgradeKube keeps pods/nodes/deployments in memory and applies
// mutations, so idempotency flows behave like a live cluster.
type mockUpgradeKube struct {
	nodes            []corev1.Node
	workerPods       []corev1.Pod
	ateletPods       []corev1.Pod
	deployments      []appsv1.Deployment
	ateletDaemonSets []appsv1.DaemonSet

	patchedLabels      []string // "node:key=value"
	deletedPods        []string // "ns/name"
	deletedDeployments []string // "ns/name"
	deletedDaemonSets  []string // "ns/name"
}

func (m *mockUpgradeKube) ListNodes(ctx context.Context) ([]corev1.Node, error) {
	return append([]corev1.Node(nil), m.nodes...), nil
}

func (m *mockUpgradeKube) PatchNodeLabel(ctx context.Context, nodeName, key, value string) error {
	m.patchedLabels = append(m.patchedLabels, fmt.Sprintf("%s:%s=%s", nodeName, key, value))
	for i := range m.nodes {
		if m.nodes[i].Name == nodeName {
			if m.nodes[i].Labels == nil {
				m.nodes[i].Labels = map[string]string{}
			}
			m.nodes[i].Labels[key] = value
		}
	}
	return nil
}

func filterPodsByNode(pods []corev1.Pod, node string) []corev1.Pod {
	if node == "" {
		return append([]corev1.Pod(nil), pods...)
	}
	var out []corev1.Pod
	for i := range pods {
		if pods[i].Spec.NodeName == node {
			out = append(out, pods[i])
		}
	}
	return out
}

func (m *mockUpgradeKube) ListWorkerPods(ctx context.Context, node string) ([]corev1.Pod, error) {
	return filterPodsByNode(m.workerPods, node), nil
}

func (m *mockUpgradeKube) ListAteletPods(ctx context.Context, node string) ([]corev1.Pod, error) {
	return filterPodsByNode(m.ateletPods, node), nil
}

func (m *mockUpgradeKube) DeletePod(ctx context.Context, namespace, name string) error {
	m.deletedPods = append(m.deletedPods, namespace+"/"+name)
	for i := range m.workerPods {
		if m.workerPods[i].Namespace == namespace && m.workerPods[i].Name == name {
			m.workerPods = append(m.workerPods[:i], m.workerPods[i+1:]...)
			break
		}
	}
	return nil
}

func (m *mockUpgradeKube) ListWorkerDeployments(ctx context.Context) ([]appsv1.Deployment, error) {
	return append([]appsv1.Deployment(nil), m.deployments...), nil
}

func (m *mockUpgradeKube) DeleteDeployment(ctx context.Context, namespace, name string) error {
	m.deletedDeployments = append(m.deletedDeployments, namespace+"/"+name)
	return nil
}

func (m *mockUpgradeKube) ListAteletDaemonSets(ctx context.Context) ([]appsv1.DaemonSet, error) {
	return append([]appsv1.DaemonSet(nil), m.ateletDaemonSets...), nil
}

func (m *mockUpgradeKube) DeleteDaemonSet(ctx context.Context, namespace, name string) error {
	m.deletedDaemonSets = append(m.deletedDaemonSets, namespace+"/"+name)
	return nil
}

func testNode(name, version string) corev1.Node {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if version != "" {
		node.Labels[versionlabel.Key] = version
	}
	return node
}

func testWorkerPod(namespace, name, node, version string, ready bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{workerPoolLabelKey: "pool"},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if version != "" {
		pod.Labels[versionlabel.Key] = version
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

func testAteletPod(name, node string, ready bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ateSystemNamespace,
			Name:      name,
			Labels:    map[string]string{"app": "atelet"},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

func testWorker(name, node, namespace, pod string) *ateapipb.Worker {
	return &ateapipb.Worker{
		Metadata:        &ateapipb.ResourceMetadata{Name: name},
		WorkerNamespace: namespace,
		WorkerPod:       pod,
		NodeName:        node,
		Status:          &ateapipb.WorkerStatus{State: ateapipb.WorkerState_WORKER_STATE_ACTIVE},
	}
}

func testActor(atespace, name string, state ateapipb.ActorState, workerName string) *ateapipb.Actor {
	a := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name},
		Status:   &ateapipb.ActorStatus{State: state},
	}
	if workerName != "" {
		a.Status.WorkerAssignment = &ateapipb.WorkerAssignment{Worker: &ateapipb.ObjectRef{Name: workerName}}
	}
	return a
}

func TestBlockingActorsOnNode(t *testing.T) {
	workers := []*ateapipb.Worker{
		testWorker("w1", "node-a", "ns1", "pod1"),
		testWorker("w2", "node-b", "ns1", "pod2"),
	}

	pausedOnA := testActor("space", "paused", ateapipb.ActorState_ACTOR_STATE_PAUSED, "")
	pausedOnA.Status.LocalSnapshotInfo = &ateapipb.LocalSnapshotInfo{NodeVmsWithLocalSnapshots: []string{"node-a"}}

	tests := []struct {
		name    string
		actors  []*ateapipb.Actor
		workers []*ateapipb.Worker
		node    string
		want    []string
	}{
		{
			name:    "running actor bound to node worker blocks",
			actors:  []*ateapipb.Actor{testActor("space", "runner", ateapipb.ActorState_ACTOR_STATE_RUNNING, "w1")},
			workers: workers,
			node:    "node-a",
			want:    []string{"space/runner (RUNNING on pod ns1/pod1)"},
		},
		{
			name:    "running actor on other node does not block",
			actors:  []*ateapipb.Actor{testActor("space", "runner", ateapipb.ActorState_ACTOR_STATE_RUNNING, "w2")},
			workers: workers,
			node:    "node-a",
			want:    nil,
		},
		{
			name:    "suspended actor does not block",
			actors:  []*ateapipb.Actor{testActor("space", "sleeper", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, "")},
			workers: workers,
			node:    "node-a",
			want:    nil,
		},
		{
			name:    "crashed actor does not block",
			actors:  []*ateapipb.Actor{testActor("space", "crashed", ateapipb.ActorState_ACTOR_STATE_CRASHED, "")},
			workers: workers,
			node:    "node-a",
			want:    nil,
		},
		{
			name:    "paused actor with local snapshot on node blocks",
			actors:  []*ateapipb.Actor{pausedOnA},
			workers: workers,
			node:    "node-a",
			want:    []string{"space/paused (PAUSED, local snapshot on node)"},
		},
		{
			name:    "paused actor with snapshot elsewhere does not block",
			actors:  []*ateapipb.Actor{pausedOnA},
			workers: workers,
			node:    "node-b",
			want:    nil,
		},
		{
			name:   "worker-reported assignment blocks even if actor list missed it",
			actors: nil,
			workers: []*ateapipb.Worker{
				func() *ateapipb.Worker {
					w := testWorker("w1", "node-a", "ns1", "pod1")
					w.Status.Assignment = &ateapipb.ActorAssignment{Actor: &ateapipb.ObjectRef{Atespace: "space", Name: "ghost"}}
					return w
				}(),
			},
			node: "node-a",
			want: []string{"space/ghost (bound to worker pod ns1/pod1)"},
		},
		{
			name: "transitional suspending actor with assignment blocks",
			actors: []*ateapipb.Actor{
				testActor("space", "leaving", ateapipb.ActorState_ACTOR_STATE_SUSPENDING, "w1"),
			},
			workers: workers,
			node:    "node-a",
			want:    []string{"space/leaving (SUSPENDING on pod ns1/pod1)"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := blockingActorsOnNode(test.actors, test.workers, test.node)
			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Errorf("blockingActorsOnNode() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSubstrateNodes(t *testing.T) {
	nodes := []corev1.Node{
		testNode("labeled", "v1"),
		testNode("has-worker", ""),
		testNode("has-atelet", ""),
		testNode("plain", ""),
	}
	workerPods := []corev1.Pod{
		testWorkerPod("ns1", "w", "has-worker", "v1", true),
		testWorkerPod("ns1", "orphan", "gone-node", "v1", true), // unknown node ignored
	}
	ateletPods := []corev1.Pod{testAteletPod("atelet-1", "has-atelet", true)}

	got := substrateNodes(nodes, workerPods, ateletPods)
	want := []string{"has-atelet", "has-worker", "labeled"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("substrateNodes() mismatch (-want +got):\n%s", diff)
	}
}
