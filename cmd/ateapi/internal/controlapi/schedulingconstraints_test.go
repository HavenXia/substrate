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

package controlapi

import (
	"slices"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/scheduling"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSchedulingConstraints_PinWinsOverTemplateSelector verifies that a pause
// actor drops the template selector from the constraints, while unpinned (regular suspended)
// constraints still honor it.
func TestSchedulingConstraints_PinWinsOverTemplateSelector(t *testing.T) {
	tmpl := &atev1alpha1.ActorTemplate{
		Spec: atev1alpha1.ActorTemplateSpec{
			SandboxClass:   atev1alpha1.SandboxClassGvisor,
			WorkerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"substrate-version": "0.2.0"}},
		},
	}
	// A worker on the pinned node whose labels do not satisfy the repointed
	// template selector.
	worker := &ateapipb.Worker{
		SandboxClass: string(atev1alpha1.SandboxClassGvisor),
		State:        ateapipb.Worker_STATE_ACTIVE,
		NodeName:     "node1",
		Labels:       map[string]string{"substrate-version": "0.1.0"},
	}

	pinned := &ateapipb.Actor{
		LocalSnapshotInfo: &ateapipb.LocalSnapshotInfo{NodeVmsWithLocalSnapshots: []string{"node1"}},
	}
	c, err := schedulingConstraints(pinned, tmpl)
	if err != nil {
		t.Fatalf("schedulingConstraints(pinned): %v", err)
	}
	if c.TemplateSelector != nil {
		t.Errorf("pinned constraints TemplateSelector = %v, want nil", c.TemplateSelector)
	}
	if want := []string{"node1"}; !slices.Equal(c.RequiredNodes, want) {
		t.Errorf("pinned constraints RequiredNodes = %v, want %v", c.RequiredNodes, want)
	}
	if !scheduling.New(nil).Applies(worker, c) {
		t.Errorf("Applies(pinned-node worker, pinned constraints) = false, want true")
	}

	unpinned := &ateapipb.Actor{}
	c, err = schedulingConstraints(unpinned, tmpl)
	if err != nil {
		t.Fatalf("schedulingConstraints(unpinned): %v", err)
	}
	if c.TemplateSelector == nil {
		t.Fatal("unpinned constraints TemplateSelector = nil, want the template selector")
	}
	if scheduling.New(nil).Applies(worker, c) {
		t.Errorf("Applies(foreign worker, unpinned constraints) = true, want false")
	}
}
