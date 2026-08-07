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

// Command harness drives the five dual-stack upgrade stories against a live
// cluster in three phases: seed (before the version bump), verify-during (run
// right after the operator enters Migrate) and verify-after (operator Ready on
// the new version). Every assertion prints a PASS/FAIL line; the run exits
// non-zero if any assertion failed. A transcript is written to
// /tmp/poc-run/transcript-<phase>.txt, and per-story counters are carried
// across phases in /tmp/poc-run/harness-state.json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/portforward"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	oldVersion = "0.1.0"
	newVersion = "0.2.0"

	templateNamespace = "ate-demo-dual"
	templateCoop      = "dualcounter"
	templateStubborn  = "dualcounter-stubborn"

	routerNamespace = "ate-system"
	routerService   = "atenet-router"

	runDir = "/tmp/poc-run"
)

type harness struct {
	atespace string
	api      *ateclient.Client
	k8s      *kubernetes.Clientset
	restCfg  *rest.Config

	mu    sync.Mutex
	out   io.Writer
	fails int

	state *runState
}

// runState carries per-story counters across phase invocations.
type runState struct {
	Counts map[string]uint64 `json:"counts"`
}

func statePath() string { return filepath.Join(runDir, "harness-state.json") }

func loadState() *runState {
	st := &runState{Counts: map[string]uint64{}}
	b, err := os.ReadFile(statePath())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, st)
	if st.Counts == nil {
		st.Counts = map[string]uint64{}
	}
	return st
}

func (h *harness) saveState() {
	b, err := json.MarshalIndent(h.state, "", "  ")
	if err == nil {
		if err := os.WriteFile(statePath(), b, 0o644); err != nil {
			h.logf("WARN: writing %s: %v", statePath(), err)
		}
	}
}

func (h *harness) logf(format string, a ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(h.out, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

func (h *harness) pass(story, format string, a ...any) {
	h.logf("PASS %s: %s", story, fmt.Sprintf(format, a...))
}

func (h *harness) fail(story, format string, a ...any) {
	h.mu.Lock()
	h.fails++
	h.mu.Unlock()
	h.logf("FAIL %s: %s", story, fmt.Sprintf(format, a...))
}

func (h *harness) ref(name string) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: h.atespace, Name: name}
}

func (h *harness) dnsName(name string) string {
	return resources.ActorRef{Atespace: h.atespace, Name: name}.DNSName()
}

// retryable classifies the control-plane races worth waiting out: per-actor
// lock contention (Aborted), worker-cache lag after a suspend freed capacity
// (FailedPrecondition "no free workers") and transport blips through the
// port-forward tunnel.
func retryable(err error) bool {
	s, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch s.Code() {
	case codes.Aborted, codes.Unavailable:
		return true
	case codes.FailedPrecondition:
		return strings.Contains(strings.ToLower(s.Message()), "no free workers")
	}
	return false
}

func (h *harness) callWithRetry(ctx context.Context, op string, timeout time.Duration, f func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := f(ctx)
		if err == nil {
			return nil
		}
		if !retryable(err) || time.Now().After(deadline) {
			return fmt.Errorf("%s: %w", op, err)
		}
		h.logf("%s: retrying: %v", op, err)
		time.Sleep(1 * time.Second)
	}
}

func (h *harness) getActor(ctx context.Context, name string) (*ateapipb.Actor, error) {
	return h.api.GetActor(ctx, &ateapipb.GetActorRequest{Actor: h.ref(name)})
}

func (h *harness) createActor(ctx context.Context, name, template string) error {
	return h.callWithRetry(ctx, "create "+name, 60*time.Second, func(ctx context.Context) error {
		_, err := h.api.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
			Metadata:               &ateapipb.ResourceMetadata{Atespace: h.atespace, Name: name},
			ActorTemplateNamespace: templateNamespace,
			ActorTemplateName:      template,
		}})
		if status.Code(err) == codes.AlreadyExists {
			h.logf("%s already exists, reusing", name)
			return nil
		}
		return err
	})
}

func (h *harness) resumeActor(ctx context.Context, name string) error {
	return h.callWithRetry(ctx, "resume "+name, 180*time.Second, func(ctx context.Context) error {
		_, err := h.api.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: h.ref(name)})
		return err
	})
}

func (h *harness) suspendActor(ctx context.Context, name string) error {
	return h.callWithRetry(ctx, "suspend "+name, 120*time.Second, func(ctx context.Context) error {
		_, err := h.api.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: h.ref(name)})
		return err
	})
}

func (h *harness) pauseActor(ctx context.Context, name string) error {
	return h.callWithRetry(ctx, "pause "+name, 120*time.Second, func(ctx context.Context) error {
		_, err := h.api.PauseActor(ctx, &ateapipb.PauseActorRequest{Actor: h.ref(name)})
		return err
	})
}

func (h *harness) waitForStatus(ctx context.Context, name string, want ateapipb.Actor_Status, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ateapipb.Actor_STATUS_UNSPECIFIED
	for time.Now().Before(deadline) {
		a, err := h.getActor(ctx, name)
		if err == nil {
			last = a.GetStatus()
			if last == want {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("%s did not reach %v within %v (last seen %v)", name, want, timeout, last)
}

// routerRequest tunnels to a ready pod behind the atenet-router door Service
// (so it always hits the active stack's routers) and issues one HTTP request
// with the actor's mesh DNS name as Host.
func (h *harness) routerRequest(ctx context.Context, method, path, name string, timeout time.Duration) (int, string, error) {
	fwCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	localPort, stop, err := portforward.ServicePortForward(fwCtx, h.restCfg, h.k8s, routerNamespace, routerService, 80)
	if err != nil {
		return 0, "", fmt.Errorf("port-forward to %s: %w", routerService, err)
	}
	defer stop()

	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", localPort, path), nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = h.dnsName(name)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}

// postActor POSTs / once and parses the returned counter value.
func (h *harness) postActor(ctx context.Context, name string, timeout time.Duration) (uint64, error) {
	code, body, err := h.routerRequest(ctx, http.MethodPost, "/", name, timeout)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("POST %s: status %d: %s", name, code, strings.TrimSpace(body))
	}
	var n uint64
	if _, err := fmt.Sscanf(body, "count: %d", &n); err != nil {
		return 0, fmt.Errorf("POST %s: unparseable body %q", name, strings.TrimSpace(body))
	}
	return n, nil
}

// postActorRetry keeps POSTing until one request lands; through the router
// this doubles as the traffic-wake path for suspended actors.
func (h *harness) postActorRetry(ctx context.Context, name string, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		n, err := h.postActor(ctx, name, 30*time.Second)
		if err == nil {
			return n, nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	return 0, fmt.Errorf("no successful POST to %s within %v: %w", name, timeout, lastErr)
}

type actorStatus struct {
	Count           uint64 `json:"count"`
	ReadyForSuspend bool   `json:"ready_for_suspend"`
	WorkInFlight    bool   `json:"work_in_flight"`
}

func (h *harness) actorStatus(ctx context.Context, name string) (*actorStatus, error) {
	code, body, err := h.routerRequest(ctx, http.MethodGet, "/status", name, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET /status %s: status %d: %s", name, code, strings.TrimSpace(body))
	}
	st := &actorStatus{}
	if err := json.Unmarshal([]byte(body), st); err != nil {
		return nil, fmt.Errorf("GET /status %s: unparseable body %q", name, strings.TrimSpace(body))
	}
	return st, nil
}

func main() {
	kubecontext := flag.String("kubecontext", "", "kubeconfig context (kubeconfig itself comes from KUBECONFIG/default)")
	atespace := flag.String("atespace", "poc", "atespace the stories live in")
	stories := flag.String("stories", "all", "comma-separated story subset (story-a..story-e) or all")
	phase := flag.String("phase", "", "seed | verify-during | verify-after")
	flag.Parse()

	switch *phase {
	case "seed", "verify-during", "verify-after":
	default:
		fmt.Fprintln(os.Stderr, "--phase must be seed, verify-during or verify-after")
		os.Exit(2)
	}

	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "creating %s: %v\n", runDir, err)
		os.Exit(2)
	}
	transcript, err := os.Create(filepath.Join(runDir, "transcript-"+*phase+".txt"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating transcript: %v\n", err)
		os.Exit(2)
	}
	defer transcript.Close()

	h := &harness{
		atespace: *atespace,
		out:      io.MultiWriter(os.Stdout, transcript),
		state:    loadState(),
	}

	selected := map[string]bool{}
	if *stories == "all" || *stories == "" {
		for _, s := range allStories {
			selected[s] = true
		}
	} else {
		for _, s := range strings.Split(*stories, ",") {
			selected[strings.TrimSpace(s)] = true
		}
	}

	ctx := context.Background()
	h.restCfg, err = ateclient.LoadConfig("", *kubecontext)
	if err != nil {
		h.logf("FATAL: loading kubeconfig: %v", err)
		os.Exit(2)
	}
	h.k8s, err = kubernetes.NewForConfig(h.restCfg)
	if err != nil {
		h.logf("FATAL: building k8s client: %v", err)
		os.Exit(2)
	}
	connectCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	h.api, err = ateclient.NewClient(connectCtx, "", *kubecontext, "", false)
	cancel()
	if err != nil {
		h.logf("FATAL: connecting to ate api: %v", err)
		os.Exit(2)
	}
	defer h.api.Close()

	h.logf("harness phase=%s atespace=%s stories=%v", *phase, *atespace, *stories)
	switch *phase {
	case "seed":
		h.runSeed(ctx, selected)
	case "verify-during":
		h.runVerifyDuring(ctx, selected)
	case "verify-after":
		h.runVerifyAfter(ctx, selected)
	}

	h.saveState()
	if h.fails > 0 {
		h.logf("RESULT: %d assertion(s) FAILED", h.fails)
		os.Exit(1)
	}
	h.logf("RESULT: all assertions passed")
}
