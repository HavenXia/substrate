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
	"encoding/json"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
)

func statusFixtures() (*mockUpgradeKube, *mockUpgradeAPI) {
	kube := &mockUpgradeKube{
		nodes: []corev1.Node{testNode("node-a", "v1"), testNode("node-b", "")},
		workerPods: []corev1.Pod{
			testWorkerPod("ns1", "old-a", "node-a", "v1", true),
			testWorkerPod("ns1", "new-a", "node-a", "v2", false),
		},
		ateletPods: []corev1.Pod{
			testAteletPod("atelet-a", "node-a", true),
			testAteletPod("atelet-b", "node-b", false),
		},
	}
	api := &mockUpgradeAPI{
		workers: []*ateapipb.Worker{testWorker("wa", "node-a", "ns1", "old-a")},
		actorsSeq: [][]*ateapipb.Actor{{
			testActor("space", "runner", ateapipb.ActorState_ACTOR_STATE_RUNNING, "wa"),
			testActor("space", "sleeper", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ""),
		}},
	}
	return kube, api
}

func TestUpgradeStatusRunner_Table(t *testing.T) {
	kube, api := statusFixtures()
	var buf bytes.Buffer
	runner := &UpgradeStatusRunner{api: api, kube: kube, outputFmt: "table", out: &buf}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	want := "NODE     VERSION   ATELET   WORKERS         ACTORS\n" +
		"node-a   v1        1/1      v1:1/1,v2:0/1   1\n" +
		"node-b   <none>    0/1      <none>          0\n"
	if diff := cmp.Diff(want, buf.String()); diff != "" {
		t.Errorf("output mismatch (-want +got):\n%s", diff)
	}
}

func TestUpgradeStatusRunner_JSON(t *testing.T) {
	kube, api := statusFixtures()
	var buf bytes.Buffer
	runner := &UpgradeStatusRunner{api: api, kube: kube, outputFmt: "json", out: &buf}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	var rows []UpgradeNodeStatus
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	want := []UpgradeNodeStatus{
		{Node: "node-a", Version: "v1", Atelet: "1/1", Workers: "v1:1/1,v2:0/1", Actors: 1},
		{Node: "node-b", Version: "", Atelet: "0/1", Workers: "<none>", Actors: 0},
	}
	if diff := cmp.Diff(want, rows); diff != "" {
		t.Errorf("rows mismatch (-want +got):\n%s", diff)
	}
}
