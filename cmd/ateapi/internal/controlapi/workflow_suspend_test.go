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
	"context"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store/ateredis"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"k8s.io/client-go/tools/cache"
)

// newTestPersistence returns a store backed by a throwaway miniredis.
func newTestPersistence(t *testing.T) store.Interface {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { rdb.Close() }) //nolint:errcheck // test cleanup
	return ateredis.NewPersistence(rdb)
}

// newDanglingDialer returns a dialer whose informer cache has no pods, so
// DialForWorker returns ErrWorkerPodNotFound.
func newDanglingDialer() *AteletDialer {
	empty := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		byNamespaceAndName: func(obj any) ([]string, error) { return nil, nil },
	})
	return NewAteletDialer(empty, empty)
}

func TestCallAteletSuspendStep_DanglingWorkerDoesNotRecordPhantomSnapshot(t *testing.T) {
	tests := []struct {
		name         string
		prevSnapshot *ateapipb.SnapshotInfo
	}{
		{
			name: "keeps previous snapshot",
			prevSnapshot: &ateapipb.SnapshotInfo{
				Data: &ateapipb.SnapshotInfo_External{
					External: &ateapipb.ExternalSnapshotInfo{SnapshotUriPrefix: "gs://snapshots/actor-1/prev"},
				},
			},
		},
		{
			name:         "stays nil without previous snapshot",
			prevSnapshot: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			persistence := newTestPersistence(t)

			actor := &ateapipb.Actor{
				Metadata:           &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-1"},
				Status:             ateapipb.Actor_STATUS_SUSPENDING,
				AteomPodNamespace:  "worker-ns",
				AteomPodName:       "pod-gone",
				WorkerPoolName:     "pool",
				InProgressSnapshot: "gs://snapshots/actor-1/never-written",
				LatestSnapshotInfo: tt.prevSnapshot,
			}
			created, err := persistence.CreateActor(ctx, actor)
			if err != nil {
				t.Fatalf("CreateActor: %v", err)
			}

			step := &CallAteletSuspendStep{store: persistence, dialer: newDanglingDialer()}
			input := &SuspendInput{ActorName: "actor-1", Atespace: "team-a"}
			if err := step.Execute(ctx, input, &SuspendState{Actor: created}); err == nil {
				t.Fatal("Execute: want error for dangling worker, got nil")
			}

			stored, err := persistence.GetActor(ctx, "team-a", "actor-1")
			if err != nil {
				t.Fatalf("GetActor: %v", err)
			}
			if stored.GetStatus() != ateapipb.Actor_STATUS_CRASHED {
				t.Errorf("status = %v, want CRASHED", stored.GetStatus())
			}
			if got := stored.GetInProgressSnapshot(); got != "gs://snapshots/actor-1/never-written" {
				t.Errorf("InProgressSnapshot = %q, want preserved for debugging", got)
			}
			if tt.prevSnapshot == nil {
				if stored.GetLatestSnapshotInfo() != nil {
					t.Errorf("LatestSnapshotInfo = %v, want nil", stored.GetLatestSnapshotInfo())
				}
			} else if got, want := stored.GetLatestSnapshotInfo().GetExternal().GetSnapshotUriPrefix(), tt.prevSnapshot.GetExternal().GetSnapshotUriPrefix(); got != want {
				t.Errorf("LatestSnapshotInfo uri = %q, want %q", got, want)
			}
		})
	}
}

func TestFinalizeSuspendedStep_ReleasesOnlyOwnWorker(t *testing.T) {
	tests := []struct {
		name               string
		assignmentAtespace string
		wantReleased       bool
	}{
		{
			name:               "frees worker assigned to this actor",
			assignmentAtespace: "team-a",
			wantReleased:       true,
		},
		{
			name:               "keeps worker assigned to same-named actor in another atespace",
			assignmentAtespace: "team-b",
			wantReleased:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			persistence := newTestPersistence(t)

			worker := &ateapipb.Worker{
				WorkerNamespace: "worker-ns",
				WorkerPool:      "pool",
				WorkerPod:       "pod-1",
				Assignment: &ateapipb.Assignment{
					Actor: &ateapipb.ObjectRef{Atespace: tt.assignmentAtespace, Name: "shared"},
				},
			}
			if err := persistence.CreateWorker(ctx, worker); err != nil {
				t.Fatalf("CreateWorker: %v", err)
			}

			actor := &ateapipb.Actor{
				Metadata:           &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "shared"},
				Status:             ateapipb.Actor_STATUS_SUSPENDING,
				AteomPodNamespace:  "worker-ns",
				AteomPodName:       "pod-1",
				WorkerPoolName:     "pool",
				InProgressSnapshot: "gs://snapshots/shared/1",
			}
			if _, err := persistence.CreateActor(ctx, actor); err != nil {
				t.Fatalf("CreateActor: %v", err)
			}

			step := &FinalizeSuspendedStep{store: persistence}
			input := &SuspendInput{ActorName: "shared", Atespace: "team-a"}
			if err := step.Execute(ctx, input, &SuspendState{}); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			stored, err := persistence.GetWorker(ctx, "worker-ns", "pool", "pod-1")
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			if released := stored.GetAssignment() == nil; released != tt.wantReleased {
				t.Errorf("worker released = %t, want %t (assignment: %v)", released, tt.wantReleased, stored.GetAssignment())
			}
		})
	}
}
