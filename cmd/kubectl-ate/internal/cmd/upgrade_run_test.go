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
		ateletPods: []corev1.Pod{
			testAteletPod("atelet-a", "node-a", true),
			testAteletPod("atelet-b", "node-b", true),
		},
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

func TestUpgradeRollRunner_WarnsWhenNoTargetPodsScheduled(t *testing.T) {
	// node-a hosts only an atelet: after the label flip no v2 worker pod will
	// ever appear (pool replicas seated elsewhere), so the runner warns and
	// moves on instead of hanging.
	kube := &mockUpgradeKube{
		nodes:      []corev1.Node{testNode("node-a", "v1")},
		ateletPods: []corev1.Pod{testAteletPod("atelet-a", "node-a", true)},
	}
	api := &mockUpgradeAPI{}
	var buf bytes.Buffer
	runner := newTestRollRunner(kube, api, "v2", &buf)
	runner.readyTimeout = 5 * time.Millisecond

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "WARNING: no v2 worker pods scheduled to node node-a") {
		t.Errorf("output missing no-pods warning:\n%s", buf.String())
	}
	if diff := cmp.Diff([]string{"node-a:ate.dev/substrate-version=v2"}, kube.patchedLabels); diff != "" {
		t.Errorf("patched labels mismatch (-want +got):\n%s", diff)
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
