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
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The five stories:
//
//	story-a: cooperative actor, live across the upgrade — drains on SIGTERM,
//	         harness suspends and resumes it onto green with RAM intact.
//	story-b: fresh actor created after the upgrade lands on green.
//	story-c: actor suspended on blue before the upgrade, woken on green.
//	story-d: stubborn actor ignores the drain and is force-killed → CRASHED.
//	story-e: actor paused on blue; the operator commits the pause to a durable
//	         snapshot (#791) and the harness resumes it on green.
const (
	storyA = "story-a"
	storyB = "story-b"
	storyC = "story-c"
	storyD = "story-d"
	storyE = "story-e"
)

var allStories = []string{storyA, storyB, storyC, storyD, storyE}

func (h *harness) ensureAtespace(ctx context.Context) error {
	_, err := h.api.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: h.atespace}},
	})
	if status.Code(err) == codes.AlreadyExists {
		return nil
	}
	return err
}

// seedRunningActor creates and resumes an actor, then POSTs it `posts` times.
// Returns the last observed count and whether every step succeeded (failures
// are already reported).
func (h *harness) seedRunningActor(ctx context.Context, story, template string, posts int) (uint64, bool) {
	if err := h.createActor(ctx, story, template); err != nil {
		h.fail(story, "create: %v", err)
		return 0, false
	}
	if err := h.resumeActor(ctx, story); err != nil {
		h.fail(story, "resume: %v", err)
		return 0, false
	}
	if err := h.waitForStatus(ctx, story, ateapipb.Actor_STATUS_RUNNING, 300*time.Second); err != nil {
		h.fail(story, "%v", err)
		return 0, false
	}
	var last uint64
	for i := 0; i < posts; i++ {
		n, err := h.postActorRetry(ctx, story, 120*time.Second)
		if err != nil {
			h.fail(story, "POST: %v", err)
			return 0, false
		}
		last = n
	}
	if last == 0 {
		h.fail(story, "count still 0 after %d POSTs", posts)
		return 0, false
	}
	return last, true
}

// startTraffic POSTs the actor every 2s through the router door and logs
// request gaps. The returned stop func cancels the loop and waits for the
// in-flight request to finish.
func (h *harness) startTraffic(ctx context.Context, story string, latest *atomic.Uint64) (stop func()) {
	tctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		lastOK := time.Now()
		inGap := false
		for {
			select {
			case <-tctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			n, err := h.postActor(tctx, story, 8*time.Second)
			if err != nil {
				if tctx.Err() != nil {
					return
				}
				if !inGap {
					h.logf("traffic %s: gap begins: %v", story, err)
					inGap = true
				}
				continue
			}
			if inGap {
				h.logf("traffic %s: recovered after %.1fs gap (count=%d)", story, time.Since(lastOK).Seconds(), n)
				inGap = false
			}
			lastOK = time.Now()
			latest.Store(n)
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func (h *harness) assertOnGreen(ctx context.Context, story string) {
	a, err := h.getActor(ctx, story)
	if err != nil {
		h.fail(story, "GetActor: %v", err)
		return
	}
	if a.GetStatus() != ateapipb.Actor_STATUS_RUNNING {
		h.fail(story, "status is %v, want STATUS_RUNNING", a.GetStatus())
		return
	}
	if v := a.GetWorkerAssignment().GetSubstrateVersion(); v != newVersion {
		h.fail(story, "assignment substrate_version=%q, want %q (worker %s)", v, newVersion, a.GetWorkerAssignment().GetWorkerPod())
		return
	}
	h.pass(story, "RUNNING on substrate_version=%s (worker %s)", newVersion, a.GetWorkerAssignment().GetWorkerPod())
}

// --- seed -------------------------------------------------------------

func (h *harness) runSeed(ctx context.Context, sel map[string]bool) {
	if err := h.ensureAtespace(ctx); err != nil {
		h.fail("seed", "CreateAtespace %q: %v", h.atespace, err)
		return
	}
	h.logf("atespace %q ready", h.atespace)

	// story-a comes up first so its traffic loop runs in the background for
	// the rest of the seeding.
	if sel[storyA] {
		if n, ok := h.seedRunningActor(ctx, storyA, templateCoop, 1); ok {
			h.state.Counts[storyA] = n
			h.pass(storyA, "seeded RUNNING (cooperative), count=%d", n)
			var latest atomic.Uint64
			latest.Store(n)
			stopTraffic := h.startTraffic(ctx, storyA, &latest)
			defer func() {
				stopTraffic()
				if l := latest.Load(); l > h.state.Counts[storyA] {
					h.state.Counts[storyA] = l
				}
				h.logf("%s: left RUNNING with traffic seen up to count=%d", storyA, h.state.Counts[storyA])
			}()
		}
	}

	if sel[storyD] {
		if n, ok := h.seedRunningActor(ctx, storyD, templateStubborn, 1); ok {
			h.pass(storyD, "seeded RUNNING (stubborn), count=%d", n)
		}
	}

	if sel[storyC] {
		if n, ok := h.seedRunningActor(ctx, storyC, templateCoop, 3); ok {
			if err := h.suspendActor(ctx, storyC); err != nil {
				h.fail(storyC, "%v", err)
			} else if err := h.waitForStatus(ctx, storyC, ateapipb.Actor_STATUS_SUSPENDED, 240*time.Second); err != nil {
				h.fail(storyC, "%v", err)
			} else {
				h.state.Counts[storyC] = n
				h.pass(storyC, "seeded SUSPENDED at count=%d (normal blue suspend)", n)
			}
		}
	}

	if sel[storyE] {
		if n, ok := h.seedRunningActor(ctx, storyE, templateCoop, 3); ok {
			if err := h.pauseActor(ctx, storyE); err != nil {
				h.fail(storyE, "%v", err)
			} else if err := h.waitForStatus(ctx, storyE, ateapipb.Actor_STATUS_PAUSED, 180*time.Second); err != nil {
				h.fail(storyE, "%v", err)
			} else {
				h.state.Counts[storyE] = n
				h.pass(storyE, "seeded PAUSED at count=%d", n)
			}
		}
	}
}

// --- verify-during ----------------------------------------------------

func (h *harness) runVerifyDuring(ctx context.Context, sel map[string]bool) {
	if !sel[storyA] {
		h.logf("story-a not selected; verify-during has nothing to do")
		return
	}

	const story = storyA
	pre := h.state.Counts[story]
	var latest atomic.Uint64
	stopTraffic := h.startTraffic(ctx, story, &latest)
	trafficStopped := false
	defer func() {
		if !trafficStopped {
			stopTraffic()
		}
	}()

	h.logf("%s: traffic running; waiting for drain-ready (operator SIGTERM), seeded count=%d", story, pre)
	deadline := time.Now().Add(10 * time.Minute)
	var statusCount uint64
	readySeen := false
	var lastRecordCheck time.Time
	for time.Now().Before(deadline) {
		st, err := h.actorStatus(ctx, story)
		if err == nil {
			statusCount = st.Count
			if st.ReadyForSuspend {
				readySeen = true
				break
			}
		}
		// The operator force-kills past its grace deadline; catch that early.
		if time.Since(lastRecordCheck) > 5*time.Second {
			lastRecordCheck = time.Now()
			if a, err := h.getActor(ctx, story); err == nil && a.GetStatus() == ateapipb.Actor_STATUS_CRASHED {
				h.fail(story, "CRASHED before reporting drain-ready (operator deadline beat the harness)")
				return
			}
		}
		time.Sleep(1 * time.Second)
	}
	if !readySeen {
		h.fail(story, "never reported ready_for_suspend")
		return
	}
	h.pass(story, "reported ready_for_suspend after SIGTERM (count=%d)", statusCount)

	stopTraffic()
	trafficStopped = true
	pre = max(pre, latest.Load(), statusCount)

	h.logf("%s: suspending (harness owns the suspend)", story)
	if err := h.suspendActor(ctx, story); err != nil {
		h.fail(story, "%v", err)
		return
	}
	if err := h.waitForStatus(ctx, story, ateapipb.Actor_STATUS_SUSPENDED, 240*time.Second); err != nil {
		h.fail(story, "%v", err)
		return
	}
	h.logf("%s: SUSPENDED; resuming (post-flip this must land on green)", story)
	if err := h.resumeActor(ctx, story); err != nil {
		h.fail(story, "%v", err)
		return
	}
	if err := h.waitForStatus(ctx, story, ateapipb.Actor_STATUS_RUNNING, 300*time.Second); err != nil {
		h.fail(story, "%v", err)
		return
	}
	h.assertOnGreen(ctx, story)

	c1, err := h.postActorRetry(ctx, story, 120*time.Second)
	if err != nil {
		h.fail(story, "POST after resume: %v", err)
		return
	}
	if c1 > pre {
		h.pass(story, "count continued across suspend/resume: %d -> %d", pre, c1)
	} else {
		h.fail(story, "count %d not > pre-suspend %d (RAM lost?)", c1, pre)
	}
	c2, err := h.postActorRetry(ctx, story, 60*time.Second)
	if err != nil {
		h.fail(story, "second POST: %v", err)
		return
	}
	if c2 > c1 {
		h.pass(story, "count monotonic after resume: %d -> %d", c1, c2)
	} else {
		h.fail(story, "count not monotonic after resume: %d then %d", c1, c2)
	}
	// The snapshot was taken after drain-ready, so restored RAM carries the
	// flag; a golden re-boot would report false.
	if st, err := h.actorStatus(ctx, story); err == nil {
		h.logf("%s: post-resume ready_for_suspend=%v (true corroborates restored memory)", story, st.ReadyForSuspend)
	}
	h.state.Counts[story] = c2
}

// --- verify-after -----------------------------------------------------

func (h *harness) runVerifyAfter(ctx context.Context, sel map[string]bool) {
	if sel[storyB] {
		h.logf("%s: creating a fresh actor post-upgrade", storyB)
		if n, ok := h.seedRunningActor(ctx, storyB, templateCoop, 1); ok {
			h.pass(storyB, "fresh actor serving, count=%d", n)
			h.assertOnGreen(ctx, storyB)
			h.state.Counts[storyB] = n
		}
	}

	if sel[storyC] {
		h.storyCAfter(ctx)
	}

	if sel[storyE] {
		h.storyEAfter(ctx)
	}

	if sel[storyD] {
		h.storyDAfter(ctx)
	}
}

func (h *harness) storyCAfter(ctx context.Context) {
	const story = storyC
	pre := h.state.Counts[story]
	if pre == 0 {
		h.logf("WARN %s: no seeded count on record; continuity check degrades to count>0", story)
	}
	h.logf("%s: traffic-wake: POSTing through the router until the parked request lands (seeded count=%d)", story, pre)
	c1, err := h.postActorRetry(ctx, story, 180*time.Second)
	if err != nil {
		h.logf("%s: traffic-wake did not land (%v); falling back to explicit ResumeActor", story, err)
		if err := h.resumeActor(ctx, story); err != nil {
			h.fail(story, "fallback resume: %v", err)
			return
		}
		if err := h.waitForStatus(ctx, story, ateapipb.Actor_STATUS_RUNNING, 300*time.Second); err != nil {
			h.fail(story, "%v", err)
			return
		}
		if c1, err = h.postActorRetry(ctx, story, 120*time.Second); err != nil {
			h.fail(story, "POST after resume: %v", err)
			return
		}
	}
	if c1 > pre {
		h.pass(story, "count preserved across the upgrade: %d -> %d", pre, c1)
	} else {
		h.fail(story, "count %d not > seeded %d (RAM lost?)", c1, pre)
	}
	c2, err := h.postActorRetry(ctx, story, 60*time.Second)
	if err != nil {
		h.fail(story, "second POST: %v", err)
		return
	}
	if c2 > c1 {
		h.pass(story, "count continues: %d -> %d", c1, c2)
	} else {
		h.fail(story, "count not continuing: %d then %d", c1, c2)
	}
	h.assertOnGreen(ctx, story)
	h.state.Counts[story] = c2
}

func (h *harness) storyEAfter(ctx context.Context) {
	const story = storyE
	pre := h.state.Counts[story]
	if a, err := h.getActor(ctx, story); err == nil {
		// Expect SUSPENDED: the operator committed the blue-pinned pause to a
		// durable snapshot (#791) during the upgrade.
		h.logf("%s: status before resume: %v (seeded count=%d)", story, a.GetStatus(), pre)
	}
	if err := h.resumeActor(ctx, story); err != nil {
		h.fail(story, "%v", err)
		return
	}
	if err := h.waitForStatus(ctx, story, ateapipb.Actor_STATUS_RUNNING, 300*time.Second); err != nil {
		h.fail(story, "%v", err)
		return
	}
	c1, err := h.postActorRetry(ctx, story, 120*time.Second)
	if err != nil {
		h.fail(story, "POST after resume: %v", err)
		return
	}
	if c1 > pre {
		h.pass(story, "count preserved across pause-commit-resume: %d -> %d", pre, c1)
	} else {
		h.fail(story, "count %d not > seeded %d (pause snapshot lost?)", c1, pre)
	}
	h.assertOnGreen(ctx, story)
	h.state.Counts[story] = c1
}

func (h *harness) storyDAfter(ctx context.Context) {
	const story = storyD
	if err := h.waitForStatus(ctx, story, ateapipb.Actor_STATUS_CRASHED, 240*time.Second); err != nil {
		h.fail(story, "%v (stubborn actor should be CRASHED after the force-kill)", err)
	} else {
		h.pass(story, "stubborn actor is CRASHED; not resuming it")
	}

	deadline := time.Now().Add(180 * time.Second)
	for {
		old, err := h.listOldWorkers(ctx)
		if err == nil && len(old) == 0 {
			h.pass(story, "no workers left with substrate-version=%s", oldVersion)
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				h.fail(story, "ListWorkers: %v", err)
			} else {
				h.fail(story, "%d worker(s) still carry substrate-version=%s: %v", len(old), oldVersion, old)
			}
			return
		}
		time.Sleep(3 * time.Second)
	}
}

func (h *harness) listOldWorkers(ctx context.Context) ([]string, error) {
	var old []string
	token := ""
	for {
		resp, err := h.api.ListWorkers(ctx, &ateapipb.ListWorkersRequest{PageToken: token})
		if err != nil {
			return nil, err
		}
		for _, w := range resp.GetWorkers() {
			if w.GetLabels()["substrate-version"] == oldVersion {
				old = append(old, w.GetWorkerNamespace()+"/"+w.GetWorkerPool()+"/"+w.GetWorkerPod())
			}
		}
		token = resp.GetNextPageToken()
		if token == "" {
			return old, nil
		}
	}
}
