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

// Command operator is the dual-live POC upgrade driver: a plain poll loop
// over the cluster-scoped Substrate CR (poc.ate.dev), run out-of-cluster
// against a kubeconfig. Apply operator/substrate-crd.yaml before starting it.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agent-substrate/substrate/internal/ateclient"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	ateclientset "github.com/agent-substrate/substrate/pkg/client/clientset/versioned"
)

func main() {
	// KUBECONFIG env / default loading rules apply; deliberately no
	// --kubeconfig flag (collides with client-go flag registration).
	kctx := flag.String("kubecontext", "", "kubeconfig context (empty = current)")
	bundleDir := flag.String("bundle-dir", "", "dir with crds.yaml/shared.yaml/stack-<v>.yaml; spec.bundleDir overrides")
	interval := flag.Duration("interval", 2*time.Second, "poll interval between reconcile passes")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := ateclient.LoadConfig("", *kctx)
	if err != nil {
		slog.Error("loading kubeconfig", "err", err)
		os.Exit(1)
	}
	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		slog.Error("building clientset", "err", err)
		os.Exit(1)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		slog.Error("building dynamic client", "err", err)
		os.Exit(1)
	}
	crd, err := ateclientset.NewForConfig(cfg)
	if err != nil {
		slog.Error("building ate CRD clientset", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var baseArgs []string
	if *kctx != "" {
		baseArgs = []string{"--context", *kctx}
	}
	r := &reconciler{
		k8s:         k8s,
		dyn:         dyn,
		crd:         crd,
		exec:        kubectlRunner{baseArgs: baseArgs},
		kubecontext: *kctx,
		bundleDir:   *bundleDir,
		rootCtx:     ctx,
		signaled:    map[string]bool{},
	}
	defer r.dropAte()

	slog.Info("dual-live operator started", "interval", *interval)
	for {
		r.tick(ctx)
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		case <-time.After(*interval):
		}
	}
}

// tick runs one reconcile pass over every Substrate CR. Passes are bounded so
// a wedged RPC cannot stall the loop forever; SuspendActor snapshot uploads
// can legitimately take minutes.
func (r *reconciler) tick(ctx context.Context) {
	pass, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	list, err := r.dyn.Resource(substrateGVR).List(pass, metav1.ListOptions{})
	if err != nil {
		slog.Warn("listing Substrate CRs (is operator/substrate-crd.yaml applied?)", "err", err)
		return
	}
	for i := range list.Items {
		r.reconcile(pass, &list.Items[i])
	}
}
