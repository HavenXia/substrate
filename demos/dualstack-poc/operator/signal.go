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
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runner abstracts the pod-exec shim (runsc kill for the signal) so tests can
// fake it. The production implementation shells out to kubectl with argv
// passed directly (no shell).
type runner interface {
	run(ctx context.Context, args ...string) (string, error)
}

type kubectlRunner struct {
	baseArgs []string // e.g. {"--context", "gke_..."}
}

func (k kubectlRunner) run(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{}, k.baseArgs...), args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// sigterm delivers a real SIGTERM into the actor's gVisor sandbox via
// kubectl-exec runsc kill — the borrowed transport until ateom signal
// forwarding (#517 remainder) lands.
func (r *reconciler) sigterm(ctx context.Context, a *ateapipb.Actor) error {
	as := a.GetWorkerAssignment()
	if as.GetWorkerPod() == "" {
		return fmt.Errorf("actor %s has no worker pod to signal", refString(a))
	}
	container, err := r.actorContainer(ctx, a)
	if err != nil {
		return err
	}
	runscPath, err := r.runscBinPath(ctx)
	if err != nil {
		return err
	}
	root := ateompath.RunSCStateDir(a.GetMetadata().GetUid())
	_, err = r.exec.run(ctx, "exec", "-n", as.GetWorkerNamespace(), as.GetWorkerPod(), "-c", "ateom", "--",
		runscPath, "-root", root, "kill", container, "TERM")
	return err
}

// actorContainer resolves the runsc container id: the template's first
// container name.
func (r *reconciler) actorContainer(ctx context.Context, a *ateapipb.Actor) (string, error) {
	t, err := r.crd.ApiV1alpha1().ActorTemplates(a.GetActorTemplateNamespace()).Get(ctx, a.GetActorTemplateName(), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading template for %s: %w", refString(a), err)
	}
	if len(t.Spec.Containers) == 0 {
		return "", fmt.Errorf("template %s/%s has no containers", a.GetActorTemplateNamespace(), a.GetActorTemplateName())
	}
	return t.Spec.Containers[0].Name, nil
}

// runscBinPath derives the on-node runsc path from the SandboxConfig CR
// (gvisor release tarball asset, or the legacy bare runsc asset). Arch is
// taken from the first node — wrong on mixed-arch pools; the per-actor truth
// is the on-node sandbox-assets record, which needs an atelet RPC.
func (r *reconciler) runscBinPath(ctx context.Context) (string, error) {
	if r.runscPath != "" {
		return r.runscPath, nil
	}
	nodes, err := r.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no nodes found")
	}
	arch := nodes.Items[0].Status.NodeInfo.Architecture

	sc, err := r.crd.ApiV1alpha1().SandboxConfigs().Get(ctx, "gvisor-default", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading SandboxConfig: %w", err)
	}
	assets := sc.Spec.Assets[arch]
	switch {
	case assets["gvisor"].SHA256 != "":
		r.runscPath = filepath.Join(ateompath.GVisorReleaseDir(assets["gvisor"].SHA256), "runsc")
	case assets["runsc"].SHA256 != "":
		r.runscPath = ateompath.RunSCBinaryPath(assets["runsc"].SHA256)
	default:
		return "", fmt.Errorf("SandboxConfig has no gvisor or runsc asset for arch %s", arch)
	}
	return r.runscPath, nil
}
