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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

var upgradeStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show per-node substrate versions, worker pods, and bound actors",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, tokenFile, traceEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to ate-api-server: %w", err)
		}
		defer apiClient.Close()

		k8sClient, err := ateclient.NewK8sClientset(kubeconfig, k8sContext)
		if err != nil {
			return fmt.Errorf("failed to create kubernetes client: %w", err)
		}

		runner := &UpgradeStatusRunner{
			api:       apiClient,
			kube:      &kubeUpgradeClient{clientset: k8sClient},
			outputFmt: outputFmt,
			out:       os.Stdout,
		}
		return runner.Run(ctx)
	},
}

func init() {
	upgradeCmd.AddCommand(upgradeStatusCmd)
}

// UpgradeNodeStatus is one node's row in the upgrade status view.
type UpgradeNodeStatus struct {
	Node string `json:"node" yaml:"node"`
	// Version is the node's substrate version label, empty when unlabeled.
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	// Atelet is ready/total atelet pods on the node, e.g. "1/1".
	Atelet string `json:"atelet" yaml:"atelet"`
	// Workers is ready/total worker pods per version, e.g. "v1:0/1,v2:2/2".
	Workers string `json:"workers" yaml:"workers"`
	// Actors is the number of actors occupying the node (bound or paused).
	Actors int `json:"actors" yaml:"actors"`
}

// UpgradeStatusRunner renders the per-node upgrade status table.
type UpgradeStatusRunner struct {
	api       upgradeAPI
	kube      upgradeKube
	outputFmt string
	out       io.Writer
}

func (r *UpgradeStatusRunner) Run(ctx context.Context) error {
	if r.out == nil {
		r.out = os.Stdout
	}

	nodes, err := r.kube.ListNodes(ctx)
	if err != nil {
		return err
	}
	workerPods, err := r.kube.ListWorkerPods(ctx, "")
	if err != nil {
		return err
	}
	ateletPods, err := r.kube.ListAteletPods(ctx, "")
	if err != nil {
		return err
	}
	workers, err := listAllWorkers(ctx, r.api)
	if err != nil {
		return err
	}
	actors, err := listAllActors(ctx, r.api)
	if err != nil {
		return err
	}

	byName := make(map[string]*corev1.Node, len(nodes))
	for i := range nodes {
		byName[nodes[i].Name] = &nodes[i]
	}

	var rows []UpgradeNodeStatus
	for _, name := range substrateNodes(nodes, workerPods, ateletPods) {
		rows = append(rows, UpgradeNodeStatus{
			Node:    name,
			Version: nodeVersion(byName[name]),
			Atelet:  readyRatio(podsOnNode(ateletPods, name)),
			Workers: workersByVersion(podsOnNode(workerPods, name)),
			Actors:  len(blockingActorsOnNode(actors, workers, name)),
		})
	}

	switch r.outputFmt {
	case "json":
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(r.out, "%s\n", b)
		return err
	case "yaml":
		b, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		yb, err := yaml.JSONToYAML(b)
		if err != nil {
			return err
		}
		_, err = r.out.Write(yb)
		return err
	case "table":
		w := tabwriter.NewWriter(r.out, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "NODE\tVERSION\tATELET\tWORKERS\tACTORS")
		for _, row := range rows {
			version := row.Version
			if version == "" {
				version = "<none>"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", row.Node, version, row.Atelet, row.Workers, row.Actors)
		}
		return w.Flush()
	default:
		return fmt.Errorf("unsupported format %q", r.outputFmt)
	}
}

func podsOnNode(pods []corev1.Pod, node string) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		if pods[i].Spec.NodeName == node {
			out = append(out, pods[i])
		}
	}
	return out
}

// readyRatio renders "ready/total" for a set of pods.
func readyRatio(pods []corev1.Pod) string {
	ready := 0
	for i := range pods {
		if isPodReady(&pods[i]) {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(pods))
}

// workersByVersion renders "version:ready/total" per version label, sorted by
// version; unlabeled pods render as "<none>:...". "<none>" alone when the
// node has no worker pods.
func workersByVersion(pods []corev1.Pod) string {
	type counts struct{ ready, total int }
	byVersion := make(map[string]*counts)
	for i := range pods {
		v := podVersion(&pods[i])
		if v == "" {
			v = "<none>"
		}
		c := byVersion[v]
		if c == nil {
			c = &counts{}
			byVersion[v] = c
		}
		c.total++
		if isPodReady(&pods[i]) {
			c.ready++
		}
	}
	if len(byVersion) == 0 {
		return "<none>"
	}
	versions := make([]string, 0, len(byVersion))
	for v := range byVersion {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	parts := make([]string, 0, len(versions))
	for _, v := range versions {
		c := byVersion[v]
		parts = append(parts, fmt.Sprintf("%s:%d/%d", v, c.ready, c.total))
	}
	return strings.Join(parts, ",")
}
