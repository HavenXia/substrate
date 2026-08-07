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

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	ateclientset "github.com/agent-substrate/substrate/pkg/client/clientset/versioned"
)

// Each pass performs at most one step of the current phase; every check is
// level-triggered, so a crash resumes from observed state.
const (
	phaseInstalling   = "Installing"
	phaseReady        = "Ready"
	phaseGreenUp      = "GreenUp"
	phaseCommitPaused = "CommitPaused"
	phaseFlip         = "Flip"
	phaseMigrate      = "Migrate"
	phaseTeardown     = "Teardown"
)

var substrateGVR = schema.GroupVersionResource{Group: "poc.ate.dev", Version: "v1alpha1", Resource: "substrates"}

const (
	ateSystemNS = "ate-system"
	demoNS      = "ate-demo-dual"
)

type reconciler struct {
	k8s         kubernetes.Interface
	dyn         dynamic.Interface
	crd         ateclientset.Interface
	exec        runner
	kubecontext string
	bundleDir   string

	// rootCtx outlives reconcile passes: the ate client's in-process
	// port-forward must not die with a pass context.
	rootCtx context.Context
	ate     *ateclient.Client

	signaled  map[string]bool // actor UID -> SIGTERM sent (in-memory; re-signal after restart is harmless)
	runscPath string          // cached; content-addressed, stable for the run
}

// crState is the typed view of the CR the reconciler works from.
type crState struct {
	Version       string // spec.version (target)
	GraceSeconds  int64
	BundleDir     string
	Phase         string
	ActiveVersion string
	NotifiedAt    string
}

func loadState(u *unstructured.Unstructured, flagBundleDir string) crState {
	s := crState{GraceSeconds: 25, BundleDir: flagBundleDir}
	s.Version, _, _ = unstructured.NestedString(u.Object, "spec", "version")
	if g, ok, _ := unstructured.NestedInt64(u.Object, "spec", "graceSeconds"); ok {
		s.GraceSeconds = g
	}
	if d, _, _ := unstructured.NestedString(u.Object, "spec", "bundleDir"); d != "" {
		s.BundleDir = d
	}
	s.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	s.ActiveVersion, _, _ = unstructured.NestedString(u.Object, "status", "activeVersion")
	s.NotifiedAt, _, _ = unstructured.NestedString(u.Object, "status", "notifiedAt")
	return s
}

// sfx converts a version to its object-name suffix: 0.1.0 -> 0-1-0.
func sfx(version string) string {
	return strings.ReplaceAll(version, ".", "-")
}

func (r *reconciler) reconcile(ctx context.Context, u *unstructured.Unstructured) {
	s := loadState(u, r.bundleDir)
	if s.Version == "" {
		slog.Warn("Substrate CR has no spec.version; skipping", "name", u.GetName())
		return
	}

	next, msg, err := r.step(ctx, u, s)
	if err != nil {
		slog.Error("phase step failed; will retry", "phase", orPending(s.Phase), "err", err)
		if perr := r.patchStatus(ctx, u, s.Phase, "retrying: "+err.Error()); perr != nil {
			slog.Error("patching retry status", "err", perr)
		}
		return
	}
	if next != s.Phase || msg != "" {
		if next != s.Phase {
			slog.Info("phase transition", "from", orPending(s.Phase), "to", next, "msg", msg)
		}
		if perr := r.patchStatus(ctx, u, next, msg); perr != nil {
			slog.Error("patching status", "err", perr)
		}
	}
}

func orPending(p string) string {
	if p == "" {
		return "Pending"
	}
	return p
}

// step runs one increment of the current phase and reports the next phase.
func (r *reconciler) step(ctx context.Context, u *unstructured.Unstructured, s crState) (next, msg string, err error) {
	old, newV := s.ActiveVersion, s.Version

	switch s.Phase {
	case "":
		if s.ActiveVersion == "" {
			return phaseInstalling, "installing " + s.Version, nil
		}
		return phaseReady, "active version " + s.ActiveVersion, nil

	case phaseInstalling:
		for _, f := range []string{"crds.yaml", "shared.yaml", "stack-" + s.Version + ".yaml"} {
			if err := r.applyBundle(ctx, s.BundleDir, f); err != nil {
				return s.Phase, "", err
			}
		}
		blockers, err := r.stackBlockers(ctx, sfx(s.Version))
		if err != nil {
			return s.Phase, "", err
		}
		shared, err := r.sharedBlockers(ctx)
		if err != nil {
			return s.Phase, "", err
		}
		blockers = append(blockers, shared...)
		if len(blockers) > 0 {
			return s.Phase, "waiting for rollout: " + strings.Join(blockers, ", "), nil
		}
		// The door (dispatcher) answering ListActors is the install-done gate.
		count, err := r.forEachActor(ctx, func(*ateapipb.Actor) error { return nil })
		if err != nil {
			return s.Phase, "", fmt.Errorf("door check: %w", err)
		}
		unstructured.SetNestedField(u.Object, s.Version, "status", "activeVersion")
		return phaseReady, fmt.Sprintf("installed %s; door serving (%d actors)", s.Version, count), nil

	case phaseReady:
		if s.ActiveVersion != "" && s.Version != s.ActiveVersion {
			return phaseGreenUp, fmt.Sprintf("upgrading %s -> %s", s.ActiveVersion, s.Version), nil
		}
		return s.Phase, "", nil

	case phaseGreenUp:
		if err := r.applyBundle(ctx, s.BundleDir, "stack-"+newV+".yaml"); err != nil {
			return s.Phase, "", err
		}
		blockers, err := r.stackBlockers(ctx, sfx(newV))
		if err != nil {
			return s.Phase, "", err
		}
		if len(blockers) > 0 {
			return s.Phase, "waiting for green rollout: " + strings.Join(blockers, ", "), nil
		}
		// Preflight: page every record to completion. Via the door (still
		// passthrough -> old stack); the read-through-green happens
		// implicitly post-flip.
		actors, err := r.forEachActor(ctx, func(*ateapipb.Actor) error { return nil })
		if err != nil {
			return s.Phase, "", fmt.Errorf("preflight ListActors: %w", err)
		}
		workers, err := r.forEachWorker(ctx, func(*ateapipb.Worker) error { return nil })
		if err != nil {
			return s.Phase, "", fmt.Errorf("preflight ListWorkers: %w", err)
		}
		return phaseCommitPaused, fmt.Sprintf("green stack up; preflight via door: %d actors, %d workers readable", actors, workers), nil

	case phaseCommitPaused:
		// Paused actors pin local snapshots to old-stack nodes; suspending
		// commits them to GCS so the pin dies before the stack does.
		var paused []*ateapipb.Actor
		if _, err := r.forEachActor(ctx, func(a *ateapipb.Actor) error {
			if a.GetStatus() == ateapipb.Actor_STATUS_PAUSED {
				paused = append(paused, a)
			}
			return nil
		}); err != nil {
			return s.Phase, "", err
		}
		if len(paused) == 0 {
			return phaseFlip, "no paused actors remain", nil
		}
		if err := r.commitPaused(ctx, u, s.Phase, paused); err != nil {
			return s.Phase, "", err
		}
		return s.Phase, fmt.Sprintf("suspending %d paused actors", len(paused)), nil

	case phaseFlip:
		ready, total, err := r.greenRoutersReady(ctx, newV)
		if err != nil {
			return s.Phase, "", err
		}
		if total == 0 || ready < total {
			return s.Phase, fmt.Sprintf("waiting for green routers: %d/%d ready", ready, total), nil
		}
		if err := r.flipRouterDoor(ctx, newV); err != nil {
			return s.Phase, "", fmt.Errorf("patching router door: %w", err)
		}
		if err := r.flipDispatcherRules(ctx); err != nil {
			return s.Phase, "", fmt.Errorf("patching dispatcher rules: %w", err)
		}
		flipped, err := r.repointTemplates(ctx, old, newV)
		if err != nil {
			return s.Phase, "", fmt.Errorf("repointing templates: %w", err)
		}
		return phaseMigrate, fmt.Sprintf("door+rules flipped, %d templates repointed; grace %ds", flipped, s.GraceSeconds), nil

	case phaseMigrate:
		anchor, perr := time.Parse(time.RFC3339, s.NotifiedAt)
		if s.NotifiedAt == "" || perr != nil {
			// An absent deadline anchor must never read as "expired".
			unstructured.SetNestedField(u.Object, time.Now().UTC().Format(time.RFC3339), "status", "notifiedAt")
			return s.Phase, fmt.Sprintf("migration window opened; grace %ds", s.GraceSeconds), nil
		}

		var runningOnOld, paused []*ateapipb.Actor
		if _, err := r.forEachActor(ctx, func(a *ateapipb.Actor) error {
			switch st := a.GetStatus(); {
			case a.GetWorkerAssignment().GetSubstrateVersion() == old &&
				(st == ateapipb.Actor_STATUS_RUNNING || st == ateapipb.Actor_STATUS_SUSPENDING ||
					st == ateapipb.Actor_STATUS_PAUSING || st == ateapipb.Actor_STATUS_RESUMING):
				runningOnOld = append(runningOnOld, a)
			case st == ateapipb.Actor_STATUS_PAUSED:
				paused = append(paused, a)
			}
			return nil
		}); err != nil {
			return s.Phase, "", err
		}
		if len(runningOnOld) == 0 {
			return phaseTeardown, "no actors left on the old stack", nil
		}

		// The eviction contract: one real SIGTERM per running actor.
		signaled := 0
		for _, a := range runningOnOld {
			if a.GetStatus() != ateapipb.Actor_STATUS_RUNNING || r.signaled[a.GetMetadata().GetUid()] {
				continue
			}
			if err := r.sigterm(ctx, a); err != nil {
				return s.Phase, "", fmt.Errorf("signaling %s: %w", refString(a), err)
			}
			r.signaled[a.GetMetadata().GetUid()] = true
			signaled++
		}

		// Pauses landing mid-window still commit to GCS.
		if len(paused) > 0 {
			if err := r.commitPaused(ctx, u, s.Phase, paused); err != nil {
				return s.Phase, "", err
			}
		}

		if time.Since(anchor) < time.Duration(s.GraceSeconds)*time.Second {
			return s.Phase, fmt.Sprintf("grace running: %d actors on old stack (%d signaled this pass)", len(runningOnOld), signaled), nil
		}

		// Past deadline: kill the worker pods. Record the intent in
		// status.killed BEFORE deleting — a crash after the delete must not
		// lose which actors were force-crashed. The blue syncer marks them
		// CRASHED and deletes the worker rows.
		for _, a := range runningOnOld {
			appendUnique(u, "killed", refString(a))
		}
		if err := r.patchStatus(ctx, u, s.Phase, fmt.Sprintf("deadline passed: deleting %d old-stack worker pods", len(runningOnOld))); err != nil {
			return s.Phase, "", err
		}
		if err := r.deleteWorkerPods(ctx, runningOnOld); err != nil {
			return s.Phase, "", err
		}
		return s.Phase, fmt.Sprintf("deadline passed: deleted %d old-stack worker pods", len(runningOnOld)), nil

	case phaseTeardown:
		// Observe-only gate: the old stack is torn down only once nothing
		// holds or serves an old assignment.
		assignedOld := 0
		if _, err := r.forEachWorker(ctx, func(w *ateapipb.Worker) error {
			if w.GetLabels()["substrate-version"] == old && w.GetAssignment() != nil {
				assignedOld++
			}
			return nil
		}); err != nil {
			return s.Phase, "", err
		}
		actorsOld := 0
		if _, err := r.forEachActor(ctx, func(a *ateapipb.Actor) error {
			if a.GetWorkerAssignment().GetSubstrateVersion() == old {
				actorsOld++
			}
			return nil
		}); err != nil {
			return s.Phase, "", err
		}
		if assignedOld > 0 || actorsOld > 0 {
			return s.Phase, fmt.Sprintf("gate not passed yet: %d assigned workers, %d actors on old stack", assignedOld, actorsOld), nil
		}
		if err := r.teardownStack(ctx, old); err != nil {
			return s.Phase, "", err
		}
		killed, _, _ := unstructured.NestedStringSlice(u.Object, "status", "killed")
		committed, _, _ := unstructured.NestedStringSlice(u.Object, "status", "committed")
		unstructured.SetNestedField(u.Object, newV, "status", "activeVersion")
		// Clear the anchor so a future upgrade cycle opens a fresh window.
		unstructured.RemoveNestedField(u.Object, "status", "notifiedAt")
		return phaseReady, fmt.Sprintf("upgrade to %s complete: %d killed, %d committed", newV, len(killed), len(committed)), nil
	}
	return s.Phase, "", fmt.Errorf("unknown phase %q", s.Phase)
}

// commitPaused write-aheads each ref into status.committed, then suspends.
// Aborted/FailedPrecondition are tolerated (lock contention or not yet
// suspendable); the next pass re-observes.
func (r *reconciler) commitPaused(ctx context.Context, u *unstructured.Unstructured, phase string, paused []*ateapipb.Actor) error {
	for _, a := range paused {
		appendUnique(u, "committed", refString(a))
	}
	if err := r.patchStatus(ctx, u, phase, fmt.Sprintf("committing %d paused actors", len(paused))); err != nil {
		return err
	}
	ate, err := r.ateClient()
	if err != nil {
		return err
	}
	for _, a := range paused {
		_, err := ate.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
			Actor: &ateapipb.ObjectRef{Atespace: a.GetMetadata().GetAtespace(), Name: a.GetMetadata().GetName()},
		})
		if err != nil {
			if c := status.Code(err); c == codes.Aborted || c == codes.FailedPrecondition {
				slog.Warn("suspend skipped; will recheck", "actor", refString(a), "err", err)
				continue
			}
			return r.ateErr(fmt.Errorf("suspending %s: %w", refString(a), err))
		}
	}
	return nil
}

// ---- status helpers ----

func (r *reconciler) patchStatus(ctx context.Context, u *unstructured.Unstructured, phase, msg string) error {
	unstructured.SetNestedField(u.Object, phase, "status", "phase")
	unstructured.SetNestedField(u.Object, msg, "status", "message")
	resp, err := r.dyn.Resource(substrateGVR).UpdateStatus(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	// Keep u current so a second write-ahead patch in the same pass does not
	// conflict with the first.
	u.Object = resp.Object
	return nil
}

func appendUnique(u *unstructured.Unstructured, field, ref string) {
	refs, _, _ := unstructured.NestedStringSlice(u.Object, "status", field)
	for _, f := range refs {
		if f == ref {
			return
		}
	}
	unstructured.SetNestedStringSlice(u.Object, append(refs, ref), "status", field)
}

func refString(a *ateapipb.Actor) string {
	return a.GetMetadata().GetAtespace() + "/" + a.GetMetadata().GetName()
}

// ---- ate-api access (via the door = dispatcher) ----

func (r *reconciler) ateClient() (*ateclient.Client, error) {
	if r.ate != nil {
		return r.ate, nil
	}
	// Empty endpoint = auto port-forward to svc api in ate-system.
	c, err := ateclient.NewClient(r.rootCtx, "", r.kubecontext, "", false)
	if err != nil {
		return nil, fmt.Errorf("dialing ate-api door: %w", err)
	}
	r.ate = c
	return c, nil
}

func (r *reconciler) dropAte() {
	if r.ate != nil {
		r.ate.Close()
		r.ate = nil
	}
}

// ateErr drops the cached client on transport-level failures so the next
// pass re-dials (the in-process port-forward pins one dispatcher pod).
func (r *reconciler) ateErr(err error) error {
	if c := status.Code(err); c == codes.Unavailable || c == codes.DeadlineExceeded || c == codes.Canceled {
		r.dropAte()
	}
	return err
}

func (r *reconciler) forEachActor(ctx context.Context, fn func(*ateapipb.Actor) error) (int, error) {
	ate, err := r.ateClient()
	if err != nil {
		return 0, err
	}
	count, token := 0, ""
	for {
		resp, err := ate.ListActors(ctx, &ateapipb.ListActorsRequest{PageToken: token})
		if err != nil {
			return count, r.ateErr(err)
		}
		for _, a := range resp.GetActors() {
			count++
			if err := fn(a); err != nil {
				return count, err
			}
		}
		token = resp.GetNextPageToken()
		if token == "" {
			return count, nil
		}
	}
}

func (r *reconciler) forEachWorker(ctx context.Context, fn func(*ateapipb.Worker) error) (int, error) {
	ate, err := r.ateClient()
	if err != nil {
		return 0, err
	}
	count, token := 0, ""
	for {
		resp, err := ate.ListWorkers(ctx, &ateapipb.ListWorkersRequest{PageToken: token})
		if err != nil {
			return count, r.ateErr(err)
		}
		for _, w := range resp.GetWorkers() {
			count++
			if err := fn(w); err != nil {
				return count, err
			}
		}
		token = resp.GetNextPageToken()
		if token == "" {
			return count, nil
		}
	}
}
