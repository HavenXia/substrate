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
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// twoNodeCluster returns a v1-labeled two-node cluster with one old (v1) and
// one new (v2, Ready) worker pod per node, plus Ready atelets.
func twoNodeCluster() (*mockUpgradeKube, *mockUpgradeAPI) {
	kube := &mockUpgradeKube{
		nodes: []corev1.Node{testNode("node-a", "v1"), testNode("node-b", "v1")},
		workerPods: []corev1.Pod{
			testWorkerPod("ns1", "old-a", "node-a", "v1", true),
			testWorkerPod("ns1", "new-a", "node-a", "v2", true),
			testWorkerPod("ns1", "old-b", "node-b", "v1", true),
			testWorkerPod("ns1", "new-b", "node-b", "v2", true),
		},
		// The mock does not emulate the DaemonSet swap, so both versions'
		// atelet pods are present and Ready; readiness picks the target one
		// in either direction. Both versioned DaemonSets are staged so
		// preflight passes in either direction too.
		ateletPods: []corev1.Pod{
			testAteletPod("atelet-a-v1", "node-a", "v1", true),
			testAteletPod("atelet-a-v2", "node-a", "v2", true),
			testAteletPod("atelet-b-v1", "node-b", "v1", true),
			testAteletPod("atelet-b-v2", "node-b", "v2", true),
		},
		ateletDaemonSets: testAteletDSs("v1", "v2"),
	}
	api := &mockUpgradeAPI{
		workers: []*ateapipb.Worker{
			testWorker("wa", "node-a", "ns1", "old-a"),
			testWorker("wna", "node-a", "ns1", "new-a"),
			testWorker("wb", "node-b", "ns1", "old-b"),
			testWorker("wnb", "node-b", "ns1", "new-b"),
		},
	}
	return kube, api
}

func newTestRollRunner(kube *mockUpgradeKube, api *mockUpgradeAPI, target string, out *bytes.Buffer) *UpgradeRollRunner {
	return &UpgradeRollRunner{
		api:           api,
		kube:          kube,
		targetVersion: target,
		pollInterval:  time.Millisecond,
		readyTimeout:  time.Second,
		out:           out,
	}
}

func TestUpgradeRollRunner_RollsNodesInOrder(t *testing.T) {
	kube, api := twoNodeCluster()
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v\noutput:\n%s", err, buf.String())
	}

	// Only the old-version workers are drained, one node at a time, ascending.
	if diff := cmp.Diff([]string{"wa", "wb"}, api.drained); diff != "" {
		t.Errorf("drained workers mismatch (-want +got):\n%s", diff)
	}
	// Only old-version pods on the node being rolled are deleted.
	if diff := cmp.Diff([]string{"ns1/old-a", "ns1/old-b"}, kube.deletedPods); diff != "" {
		t.Errorf("deleted pods mismatch (-want +got):\n%s", diff)
	}
	want := []string{
		"node-a:ate.dev/substrate-version=v2",
		"node-b:ate.dev/substrate-version=v2",
	}
	if diff := cmp.Diff(want, kube.patchedLabels); diff != "" {
		t.Errorf("patched labels mismatch (-want +got):\n%s", diff)
	}
	// The label flips before the old pods are deleted: post-flip replacements
	// from the old ReplicaSet can no longer bind to the node.
	wantOps := []string{
		"patch:node-a", "delete:ns1/old-a",
		"patch:node-b", "delete:ns1/old-b",
	}
	if diff := cmp.Diff(wantOps, kube.ops); diff != "" {
		t.Errorf("mutation order mismatch (-want +got):\n%s", diff)
	}
}

func TestUpgradeRollRunner_RollbackReversesNodeOrder(t *testing.T) {
	kube, api := twoNodeCluster()
	// Start from the rolled state: nodes at v2, v1 pods are the spring.
	for i := range kube.nodes {
		kube.nodes[i].Labels["ate.dev/substrate-version"] = "v2"
	}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v1", &buf)
	runner.rollback = true

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v\noutput:\n%s", err, buf.String())
	}

	want := []string{
		"node-b:ate.dev/substrate-version=v1",
		"node-a:ate.dev/substrate-version=v1",
	}
	if diff := cmp.Diff(want, kube.patchedLabels); diff != "" {
		t.Errorf("patched labels mismatch (-want +got):\n%s", diff)
	}
	// Rolling back deletes the v2 pods.
	if diff := cmp.Diff([]string{"ns1/new-b", "ns1/new-a"}, kube.deletedPods); diff != "" {
		t.Errorf("deleted pods mismatch (-want +got):\n%s", diff)
	}
}

func TestUpgradeRollRunner_ExplicitNodes(t *testing.T) {
	kube, api := twoNodeCluster()
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.nodes = []string{"node-b", "node-a"}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	want := []string{
		"node-b:ate.dev/substrate-version=v2",
		"node-a:ate.dev/substrate-version=v2",
	}
	if diff := cmp.Diff(want, kube.patchedLabels); diff != "" {
		t.Errorf("patched labels mismatch (-want +got):\n%s", diff)
	}
}

func TestUpgradeRollRunner_UnknownNode(t *testing.T) {
	kube, api := twoNodeCluster()
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.nodes = []string{"nope"}

	err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `node "nope" not found`) {
		t.Fatalf("Run() error = %v, want unknown-node error", err)
	}
}

func TestUpgradeRollRunner_WaitsForRunningAndPausedActors(t *testing.T) {
	kube, api := twoNodeCluster()
	kube.nodes = kube.nodes[:1] // node-a only
	paused := testActor("space", "napper", ateapipb.ActorState_ACTOR_STATE_PAUSED, "")
	paused.Status.LocalSnapshotInfo = &ateapipb.LocalSnapshotInfo{NodeVmsWithLocalSnapshots: []string{"node-a"}}
	api.actorsSeq = [][]*ateapipb.Actor{
		// Poll 1: a running actor still occupies the node.
		{testActor("space", "runner", ateapipb.ActorState_ACTOR_STATE_RUNNING, "wa")},
		// Poll 2: only a paused actor's local snapshot remains; still blocks.
		{paused},
		// Poll 3+: only a suspended actor; does not block.
		{testActor("space", "sleeper", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, "")},
	}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.nodes = []string{"node-a"}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	if api.listActorsCalls != 3 {
		t.Errorf("ListActors calls = %d, want exactly 3 (suspended actor must not block)", api.listActorsCalls)
	}
	if diff := cmp.Diff([]string{"ns1/old-a"}, kube.deletedPods); diff != "" {
		t.Errorf("deleted pods mismatch (-want +got):\n%s", diff)
	}
	out := buf.String()
	if !strings.Contains(out, "space/runner (RUNNING on pod ns1/old-a)") {
		t.Errorf("output missing running-actor wait line:\n%s", out)
	}
	if !strings.Contains(out, "space/napper (PAUSED, local snapshot on node)") {
		t.Errorf("output missing paused-actor wait line:\n%s", out)
	}
}

func TestUpgradeRollRunner_DrainTimeout(t *testing.T) {
	kube, api := twoNodeCluster()
	api.actorsSeq = [][]*ateapipb.Actor{
		{testActor("space", "runner", ateapipb.ActorState_ACTOR_STATE_RUNNING, "wa")},
	}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.nodes = []string{"node-a"}
	runner.drainTimeout = 5 * time.Millisecond

	err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rerun to keep waiting") {
		t.Fatalf("Run() error = %v, want drain-timeout error", err)
	}
	if len(kube.deletedPods) != 0 {
		t.Errorf("deleted pods = %v, want none while actors block", kube.deletedPods)
	}
	if len(kube.patchedLabels) != 0 {
		t.Errorf("patched labels = %v, want none while actors block", kube.patchedLabels)
	}
}

func TestUpgradeRollRunner_DrainFailureAborts(t *testing.T) {
	// Any DrainWorker failure aborts before the cluster is touched: rolling a
	// node whose workers still accept new actors would race the drain gate.
	kube, api := twoNodeCluster()
	api.drainErr = status.Error(codes.Unimplemented, "DrainWorker is not implemented yet")
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)

	err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failed to drain worker") {
		t.Fatalf("Run() error = %v, want drain error", err)
	}
	if len(kube.deletedPods) != 0 || len(kube.patchedLabels) != 0 {
		t.Errorf("cluster mutated despite abort: deleted=%v patched=%v", kube.deletedPods, kube.patchedLabels)
	}
}

func TestUpgradeRollRunner_IdempotentRerunSkipsDoneNodes(t *testing.T) {
	kube, api := twoNodeCluster()
	var buf bytes.Buffer
	if err := newTestRollRunner(kube, api, "v2", &buf).Run(context.Background()); err != nil {
		t.Fatalf("first Run() unexpected error: %v", err)
	}

	// Second run: everything is already at v2.
	api2 := &mockUpgradeAPI{workers: api.workers}
	var buf2 bytes.Buffer
	if err := newTestRollRunner(kube, api2, "v2", &buf2).Run(context.Background()); err != nil {
		t.Fatalf("second Run() unexpected error: %v\noutput:\n%s", err, buf2.String())
	}
	if len(api2.drained) != 0 {
		t.Errorf("second run drained %v, want none", api2.drained)
	}
	if api2.listActorsCalls != 0 {
		t.Errorf("second run polled actors %d times, want 0", api2.listActorsCalls)
	}
	if got := kube.deletedPods; len(got) != 2 {
		t.Errorf("second run deleted more pods: %v", got)
	}
	if got := kube.patchedLabels; len(got) != 2 {
		t.Errorf("second run patched more labels: %v", got)
	}
	if got := strings.Count(buf2.String(), "already at v2"); got != 2 {
		t.Errorf("second run output has %d skip lines, want 2:\n%s", got, buf2.String())
	}
}

func TestUpgradeRollRunner_AteletOnlyNodeSkipsWorkerWait(t *testing.T) {
	// node-a hosts only an atelet, no old worker pods: it owes the roll no
	// replacement capacity, so once the flipped node's atelet is Ready the
	// runner moves on immediately instead of waiting out the ready timeout
	// for worker pods that may never come.
	kube := &mockUpgradeKube{
		nodes:            []corev1.Node{testNode("node-a", "v1")},
		ateletPods:       []corev1.Pod{testAteletPod("atelet-a", "node-a", "v2", true)},
		ateletDaemonSets: testAteletDSs("v2"),
	}
	api := &mockUpgradeAPI{}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.readyTimeout = time.Hour // must not be waited out

	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() unexpected error: %v\noutput:\n%s", err, buf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() still waiting; atelet-only node should not wait for worker pods")
	}
	if !strings.Contains(buf.String(), "capacity is seated elsewhere") {
		t.Errorf("output missing atelet-only fast path line:\n%s", buf.String())
	}
	if diff := cmp.Diff([]string{"node-a:ate.dev/substrate-version=v2"}, kube.patchedLabels); diff != "" {
		t.Errorf("patched labels mismatch (-want +got):\n%s", diff)
	}
}

func TestUpgradeRollRunner_RollbackOverEmptiedFleetStillWaitsForWorkers(t *testing.T) {
	// A failed upgrade can leave nodes with zero seated worker pods and the
	// old version's replacements Pending unscheduled. Rolling back, those
	// nodes have no oldPods, but the queued v1 pods are exactly the capacity
	// the rollback exists to restore: the fast path must not skip the wait.
	pendingV1 := testWorkerPod("ns1", "spring-1", "", "v1", false)
	pendingV1.Status.Phase = corev1.PodPending
	kube := &mockUpgradeKube{
		nodes: []corev1.Node{testNode("node-a", "v2")},
		workerPods: []corev1.Pod{
			pendingV1, // unscheduled: spec.nodeName is empty
		},
		ateletPods: []corev1.Pod{
			testAteletPod("atelet-a-v1", "node-a", "v1", true),
			testAteletPod("atelet-a-v2", "node-a", "v2", true),
		},
		ateletDaemonSets: testAteletDSs("v1", "v2"),
	}
	api := &mockUpgradeAPI{}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v1", &buf)
	runner.rollback = true
	runner.readyTimeout = 5 * time.Millisecond

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "capacity is seated elsewhere") {
		t.Errorf("rollback took the fast path with queued v1 pods:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "WARNING: v1 worker pods are still unscheduled") {
		t.Errorf("output missing the bounded queued-pods wait:\n%s", buf.String())
	}
}

func TestUpgradeRollRunner_RerunIgnoresActorsOnTargetWorkers(t *testing.T) {
	// Partially rolled node: label already at v2, the v2 worker serves an
	// actor, and a stray v1 pod is left over. The rerun must not wait for the
	// v2 worker's actor; it only drains and deletes the stray old pod.
	kube := &mockUpgradeKube{
		nodes: []corev1.Node{testNode("node-a", "v2")},
		workerPods: []corev1.Pod{
			testWorkerPod("ns1", "old-a", "node-a", "v1", true),
			testWorkerPod("ns1", "new-a", "node-a", "v2", true),
		},
		ateletPods:       []corev1.Pod{testAteletPod("atelet-a", "node-a", "v2", true)},
		ateletDaemonSets: testAteletDSs("v2"),
	}
	api := &mockUpgradeAPI{
		workers: []*ateapipb.Worker{
			testWorker("wa", "node-a", "ns1", "old-a"),
			testWorker("wna", "node-a", "ns1", "new-a"),
		},
		actorsSeq: [][]*ateapipb.Actor{
			{testActor("space", "busy", ateapipb.ActorState_ACTOR_STATE_RUNNING, "wna")},
		},
	}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.drainTimeout = 50 * time.Millisecond // would trip if the v2 actor blocked

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	if diff := cmp.Diff([]string{"wa"}, api.drained); diff != "" {
		t.Errorf("drained workers mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"ns1/old-a"}, kube.deletedPods); diff != "" {
		t.Errorf("deleted pods mismatch (-want +got):\n%s", diff)
	}
	if len(kube.patchedLabels) != 0 {
		t.Errorf("patched labels = %v, want none (node already at v2)", kube.patchedLabels)
	}
}

func TestUpgradeRollRunner_ContinuesWhenNodeCannotHostAtelet(t *testing.T) {
	// A node the atelet DaemonSet cannot schedule to (e.g. tainted) must not
	// abort the roll: warn at the deadline and continue.
	kube := &mockUpgradeKube{
		nodes: []corev1.Node{testNode("node-a", "v1")},
		workerPods: []corev1.Pod{
			testWorkerPod("ns1", "old-a", "node-a", "v1", true),
			testWorkerPod("ns1", "new-a", "node-a", "v2", true),
		},
		ateletDaemonSets: testAteletDSs("v2"),
	}
	api := &mockUpgradeAPI{workers: []*ateapipb.Worker{testWorker("wa", "node-a", "ns1", "old-a")}}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.readyTimeout = 5 * time.Millisecond

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "WARNING: no atelet pod on node node-a") {
		t.Errorf("output missing no-atelet warning:\n%s", buf.String())
	}
	if diff := cmp.Diff([]string{"ns1/old-a"}, kube.deletedPods); diff != "" {
		t.Errorf("deleted pods mismatch (-want +got):\n%s", diff)
	}
}

func TestAteletReady(t *testing.T) {
	tests := []struct {
		name string
		pods []corev1.Pod
		want bool
	}{
		{
			name: "target-version pod ready",
			pods: []corev1.Pod{testAteletPod("a", "n", "v2", true)},
			want: true,
		},
		{
			name: "unlabeled pre-versioning pod does not count",
			pods: []corev1.Pod{testAteletPod("a", "n", "", true)},
		},
		{
			name: "other-version pod ready does not count",
			pods: []corev1.Pod{testAteletPod("a", "n", "v1", true)},
		},
		{
			name: "target-version pod unready",
			pods: []corev1.Pod{testAteletPod("a", "n", "v2", false)},
		},
		{
			name: "no pods",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ateletReady(test.pods, "v2"); got != test.want {
				t.Errorf("ateletReady() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestUpgradeRollRunner_FailsWhenTargetPodsNotReady(t *testing.T) {
	kube, api := twoNodeCluster()
	// Make node-a's v2 pod permanently unready.
	for i := range kube.workerPods {
		if kube.workerPods[i].Name == "new-a" {
			kube.workerPods[i].Status.Conditions = nil
		}
	}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.nodes = []string{"node-a"}
	runner.readyTimeout = 5 * time.Millisecond

	err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not ready after") {
		t.Fatalf("Run() error = %v, want readiness-timeout error", err)
	}
}

func TestUpgradeRollRunner_PreflightRequiresTargetAteletDS(t *testing.T) {
	// Refuse before touching any node when the target version's DaemonSet is
	// not staged, or the label flip would leave converted nodes ateletless. An
	// unlabeled pre-versioning DaemonSet does not count: its immutable
	// selector can never gain the version key, so those clusters are
	// fresh-install territory, not roll territory.
	tests := []struct {
		name string
		dss  []appsv1.DaemonSet
	}{
		{name: "no atelet DaemonSet at all"},
		{name: "only an unlabeled pre-versioning DaemonSet", dss: testAteletDSs("")},
		{name: "only another version's DaemonSet", dss: testAteletDSs("v1")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kube := &mockUpgradeKube{
				nodes:            []corev1.Node{testNode("node-a", "v1")},
				ateletDaemonSets: test.dss,
			}
			api := &mockUpgradeAPI{}
			var buf bytes.Buffer
			runner := newTestRollRunner(kube, api, "v2", &buf)

			err := runner.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "atelet DaemonSet") {
				t.Fatalf("Run() error = %v, want missing atelet DaemonSet error", err)
			}
			if len(kube.patchedLabels) != 0 {
				t.Errorf("patched labels = %v, want none when preflight fails", kube.patchedLabels)
			}
		})
	}
}
