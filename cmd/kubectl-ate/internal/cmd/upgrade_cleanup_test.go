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

	"github.com/agent-substrate/substrate/internal/versionlabel"
	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testDeployment(namespace, name, version string) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				workerPoolLabelKey: "pool",
				versionlabel.Key:   version,
			},
		},
	}
}

func TestUpgradeCleanupRunner_DeletesOnlyTargetVersion(t *testing.T) {
	spring := testWorkerPod("ns1", "old-pending", "", "v1", false)
	spring.Status.Phase = corev1.PodPending
	kube := &mockUpgradeKube{
		nodes: []corev1.Node{testNode("node-a", "v2")},
		deployments: []appsv1.Deployment{
			testDeployment("ns1", "pool-v1", "v1"),
			testDeployment("ns1", "pool-v2", "v2"),
		},
		ateletDaemonSets: []appsv1.DaemonSet{
			{ObjectMeta: metav1.ObjectMeta{Namespace: ateSystemNamespace, Name: "atelet-v1", Labels: map[string]string{"app": "atelet", versionlabel.Key: "v1"}}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: ateSystemNamespace, Name: "atelet-v2", Labels: map[string]string{"app": "atelet", versionlabel.Key: "v2"}}},
		},
		workerPods: []corev1.Pod{
			spring, // Pending pods do not count as running.
			testWorkerPod("ns1", "new-a", "node-a", "v2", true),
		},
	}
	var buf bytes.Buffer
	runner := &UpgradeCleanupRunner{kube: kube, version: "v1", out: &buf}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if diff := cmp.Diff([]string{"ns1/pool-v1"}, kube.deletedDeployments); diff != "" {
		t.Errorf("deleted deployments mismatch (-want +got):\n%s", diff)
	}
	// The retired version's atelet DaemonSet goes with its worker sets; the
	// live version's stays.
	if diff := cmp.Diff([]string{ateSystemNamespace + "/atelet-v1"}, kube.deletedDaemonSets); diff != "" {
		t.Errorf("deleted daemonsets mismatch (-want +got):\n%s", diff)
	}
	if !strings.Contains(buf.String(), `Retired version "v1": 1 worker Deployment(s), 1 atelet DaemonSet(s)`) {
		t.Errorf("output missing summary:\n%s", buf.String())
	}
}

func TestUpgradeCleanupRunner_RefusesWhilePodsRunning(t *testing.T) {
	kube := &mockUpgradeKube{
		nodes:       []corev1.Node{testNode("node-a", "v2")},
		deployments: []appsv1.Deployment{testDeployment("ns1", "pool-v1", "v1")},
		workerPods:  []corev1.Pod{testWorkerPod("ns1", "old-a", "node-a", "v1", true)},
	}
	var buf bytes.Buffer
	runner := &UpgradeCleanupRunner{kube: kube, version: "v1", out: &buf}

	err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("Run() error = %v, want running-pods refusal", err)
	}
	if len(kube.deletedDeployments) != 0 {
		t.Errorf("deleted deployments = %v, want none", kube.deletedDeployments)
	}
}

func TestUpgradeCleanupRunner_RefusesWhileNodesLabeled(t *testing.T) {
	kube := &mockUpgradeKube{
		nodes:       []corev1.Node{testNode("node-a", "v1")},
		deployments: []appsv1.Deployment{testDeployment("ns1", "pool-v1", "v1")},
	}
	var buf bytes.Buffer
	runner := &UpgradeCleanupRunner{kube: kube, version: "v1", out: &buf}

	err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still labeled") {
		t.Fatalf("Run() error = %v, want labeled-nodes refusal", err)
	}
	if len(kube.deletedDeployments) != 0 {
		t.Errorf("deleted deployments = %v, want none", kube.deletedDeployments)
	}
}

func TestUpgradeCleanupRunner_NoDeploymentsAtVersion(t *testing.T) {
	kube := &mockUpgradeKube{
		nodes:       []corev1.Node{testNode("node-a", "v2")},
		deployments: []appsv1.Deployment{testDeployment("ns1", "pool-v2", "v2")},
	}
	var buf bytes.Buffer
	runner := &UpgradeCleanupRunner{kube: kube, version: "v1", out: &buf}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `Nothing left at version "v1"`) {
		t.Errorf("output missing no-op message:\n%s", buf.String())
	}
	if len(kube.deletedDeployments) != 0 {
		t.Errorf("deleted deployments = %v, want none", kube.deletedDeployments)
	}
}
