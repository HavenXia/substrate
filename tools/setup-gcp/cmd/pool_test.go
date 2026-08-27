// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestPoolLabelPlan(t *testing.T) {
	tests := []struct {
		name         string
		existing     map[string]string
		version      string
		onlyIfAbsent bool
		want         map[string]string
		wantAction   poolLabelAction
	}{
		{
			name:     "preserves other labels",
			existing: map[string]string{"ate.dev/sandboxClass": "gvisor", "ate.dev/substrate-version": "v1"},
			version:  "v2",
			want: map[string]string{
				"ate.dev/sandboxClass":      "gvisor",
				"ate.dev/substrate-version": "v2",
			},
			wantAction: poolLabelSet,
		},
		{
			// GetNodePool reports GKE-managed labels, but UpdateNodePool
			// rejects any request naming one (seen live: sandbox.gke.io/runtime
			// on gVisor pools). They must not be written back.
			name: "drops GKE-managed keys from the write set",
			existing: map[string]string{
				"sandbox.gke.io/runtime":       "gvisor",
				"cloud.google.com/gke-preempt": "true",
				"ate.dev/sandboxClass":         "gvisor",
			},
			version: "v1",
			want: map[string]string{
				"ate.dev/sandboxClass":      "gvisor",
				"ate.dev/substrate-version": "v1",
			},
			wantAction: poolLabelSet,
		},
		{
			name:       "adds to unlabeled pool",
			existing:   nil,
			version:    "v1",
			want:       map[string]string{"ate.dev/substrate-version": "v1"},
			wantAction: poolLabelSet,
		},
		{
			name:       "no-op when already set",
			existing:   map[string]string{"ate.dev/substrate-version": "v2"},
			version:    "v2",
			want:       map[string]string{"ate.dev/substrate-version": "v2"},
			wantAction: poolLabelNoop,
		},
		{
			name:         "only-if-absent adds to unlabeled pool",
			existing:     map[string]string{"ate.dev/sandboxClass": "gvisor"},
			version:      "v1",
			onlyIfAbsent: true,
			want: map[string]string{
				"ate.dev/sandboxClass":      "gvisor",
				"ate.dev/substrate-version": "v1",
			},
			wantAction: poolLabelSet,
		},
		{
			// Install-time stamping must never move a live fleet: the pool
			// update applies in place to every existing node.
			name:         "only-if-absent leaves a different version alone",
			existing:     map[string]string{"ate.dev/substrate-version": "v1"},
			version:      "v2",
			onlyIfAbsent: true,
			want:         map[string]string{"ate.dev/substrate-version": "v1"},
			wantAction:   poolLabelSkip,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, action := poolLabelPlan(test.existing, test.version, test.onlyIfAbsent)
			if diff := cmp.Diff(test.want, got); diff != "" {
				t.Errorf("labels mismatch (-want +got):\n%s", diff)
			}
			if action != test.wantAction {
				t.Errorf("action = %v, want %v", action, test.wantAction)
			}
		})
	}
}

func TestAutoscalingJSONRoundTrip(t *testing.T) {
	// The get-autoscaling/set-autoscaling --from-json pair must round-trip
	// the FULL message: pools autoscaled with total_* limits or a location
	// policy carry zero per-zone min/max, and a lossy round-trip would
	// restore them as disabled or mis-limited.
	in := &containerpb.NodePoolAutoscaling{
		Enabled:           true,
		TotalMinNodeCount: 2,
		TotalMaxNodeCount: 9,
		LocationPolicy:    containerpb.NodePoolAutoscaling_ANY,
	}
	b, err := protojson.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := &containerpb.NodePoolAutoscaling{}
	if err := protojson.Unmarshal(b, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !proto.Equal(in, out) {
		t.Errorf("round trip lost fields: in=%v out=%v", in, out)
	}
}
