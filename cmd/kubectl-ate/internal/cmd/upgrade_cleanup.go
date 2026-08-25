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
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/versionlabel"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
)

var upgradeCleanupVersion string

var upgradeCleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Retire a substrate version: delete its worker Deployments and atelet DaemonSet",
	Long: `Deletes the version-keyed worker Deployments and the versioned atelet
DaemonSet left behind after a completed roll. The controller never deletes
sets of another version (hands-off), so this is the only way they go away.
Refuses if the version still has running worker pods or nodes labeled with
it, since those objects are the rollback path.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		k8sClient, err := ateclient.NewK8sClientset(kubeconfig, k8sContext)
		if err != nil {
			return fmt.Errorf("failed to create kubernetes client: %w", err)
		}
		runner := &UpgradeCleanupRunner{
			kube:    &kubeUpgradeClient{clientset: k8sClient},
			version: upgradeCleanupVersion,
			out:     os.Stdout,
		}
		return runner.Run(cmd.Context())
	},
}

func init() {
	upgradeCleanupCmd.Flags().StringVar(&upgradeCleanupVersion, "version", "", "Substrate build version to retire (worker Deployments and atelet DaemonSet)")
	_ = upgradeCleanupCmd.MarkFlagRequired("version")
	upgradeCmd.AddCommand(upgradeCleanupCmd)
}

// UpgradeCleanupRunner deletes the worker Deployments and the atelet
// DaemonSet of a retired version.
type UpgradeCleanupRunner struct {
	kube    upgradeKube
	version string
	out     io.Writer
}

func (r *UpgradeCleanupRunner) Run(ctx context.Context) error {
	if r.out == nil {
		r.out = os.Stdout
	}
	if r.version == "" {
		return fmt.Errorf("--version is required")
	}
	target := versionlabel.Value(r.version)

	// A node still labeled with this version is either mid-roll or a rollback
	// target; deleting the Deployments now would destroy the rollback spring.
	nodes, err := r.kube.ListNodes(ctx)
	if err != nil {
		return err
	}
	var labeledNodes []string
	for i := range nodes {
		if nodeVersion(&nodes[i]) == target {
			labeledNodes = append(labeledNodes, nodes[i].Name)
		}
	}
	if len(labeledNodes) > 0 {
		return fmt.Errorf("refusing cleanup: %d node(s) still labeled %s=%s: %s", len(labeledNodes), versionlabel.Key, target, strings.Join(labeledNodes, ", "))
	}

	deployments, err := r.kube.ListWorkerDeployments(ctx)
	if err != nil {
		return err
	}
	var targets []int
	for i := range deployments {
		if deployments[i].Labels[versionlabel.Key] == target {
			targets = append(targets, i)
		}
	}
	daemonSets, err := r.kube.ListAteletDaemonSets(ctx)
	if err != nil {
		return err
	}
	var dsTargets []int
	for i := range daemonSets {
		if daemonSets[i].Labels[versionlabel.Key] == target {
			dsTargets = append(dsTargets, i)
		}
	}
	if len(targets) == 0 && len(dsTargets) == 0 {
		fmt.Fprintf(r.out, "Nothing left at version %q\n", target)
		return nil
	}

	pods, err := r.kube.ListWorkerPods(ctx, "")
	if err != nil {
		return err
	}
	var running []string
	for i := range pods {
		if podVersion(&pods[i]) != target {
			continue
		}
		if phase := pods[i].Status.Phase; phase == "" || phase == corev1.PodRunning || phase == corev1.PodUnknown {
			running = append(running, pods[i].Namespace+"/"+pods[i].Name)
		}
	}
	if len(running) > 0 {
		return fmt.Errorf("refusing cleanup: %d worker pod(s) at version %q still running: %s", len(running), target, summarizeList(running, 5))
	}

	for _, i := range targets {
		d := &deployments[i]
		fmt.Fprintf(r.out, "deleting worker Deployment %s/%s (version %q)\n", d.Namespace, d.Name, target)
		if err := r.kube.DeleteDeployment(ctx, d.Namespace, d.Name); err != nil {
			return fmt.Errorf("failed to delete Deployment %s/%s: %w", d.Namespace, d.Name, err)
		}
	}
	// The retired version's atelet DaemonSet goes with its worker sets: no
	// node carries the label anymore (guard above), so the DaemonSet runs
	// zero pods and only exists to serve a rollback, which retiring forecloses.
	for _, i := range dsTargets {
		ds := &daemonSets[i]
		fmt.Fprintf(r.out, "deleting atelet DaemonSet %s/%s (version %q)\n", ds.Namespace, ds.Name, target)
		if err := r.kube.DeleteDaemonSet(ctx, ds.Namespace, ds.Name); err != nil {
			return fmt.Errorf("failed to delete DaemonSet %s/%s: %w", ds.Namespace, ds.Name, err)
		}
	}
	fmt.Fprintf(r.out, "Retired version %q: %d worker Deployment(s), %d atelet DaemonSet(s)\n", target, len(targets), len(dsTargets))
	return nil
}
