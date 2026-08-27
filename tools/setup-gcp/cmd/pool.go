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
	"context"
	"fmt"
	"log/slog"
	"strings"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/agent-substrate/substrate/internal/versionlabel"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
)

// Node pool operations for the rolling dataplane upgrade. The pool's
// Kubernetes node labels are the birth default for every node the pool
// produces (autoscaling, resize, auto-repair, auto-upgrade), so the upgrade
// runbook moves the pool's ate.dev/substrate-version label only AFTER the
// per-node roll: the GKE API applies pool label updates in place to all
// existing nodes, which before the roll would flip the whole fleet at once.
// Autoscaling is disabled for the roll window so the retired side's Pending
// pods (kept for rollback) cannot re-inflate it, and restored afterwards.

var (
	poolVersion         string
	poolVersionIfAbsent bool
	poolAutoscalingOn   bool
	poolAutoscalingMin  int32
	poolAutoscalingMax  int32
	poolAutoscalingJSON string
)

var poolCmd = &cobra.Command{
	Use:   "pool",
	Short: "Operate on the substrate node pool (version label, autoscaling)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var poolSetVersionLabelCmd = &cobra.Command{
	Use:   "set-version-label",
	Short: "Set the pool's " + versionlabel.Key + " label, preserving all other labels",
	Long: `Sets the node pool's ` + versionlabel.Key + ` Kubernetes node label to the
given version. The GKE UpdateNodePool call replaces the FULL user label set
and applies it in place to every existing node in the pool, so this command
reads the current labels first and merges. Run it only after a completed
roll: before that it would flip every node at once.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return setPoolVersionLabel(cmd.Context(), &cfg, poolVersion, poolVersionIfAbsent)
	},
}

var poolGetAutoscalingCmd = &cobra.Command{
	Use:   "get-autoscaling",
	Short: "Print the pool's full NodePoolAutoscaling as one-line JSON",
	Long: `Prints the pool's NodePoolAutoscaling message as protojson, suitable for an
exact restore with set-autoscaling --from-json. The full message matters:
pools can be autoscaled with total_* limits or a location policy instead of
per-zone min/max, and a lossy round-trip would drop them.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return printPoolAutoscaling(cmd.Context(), &cfg)
	},
}

var poolSetAutoscalingCmd = &cobra.Command{
	Use:   "set-autoscaling",
	Short: "Enable or disable the pool's autoscaler (disable for the roll window)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return setPoolAutoscaling(cmd.Context(), &cfg, poolAutoscalingOn, poolAutoscalingMin, poolAutoscalingMax, poolAutoscalingJSON)
	},
}

func init() {
	poolCmd.PersistentFlags().StringVar(&cfg.ClusterName, "cluster-name", getEnv("CLUSTER_NAME", "substrate-poc"), "Name of the GKE cluster [env: CLUSTER_NAME]")
	poolCmd.PersistentFlags().StringVar(&cfg.ClusterLocation, "cluster-location", getEnv("CLUSTER_LOCATION", "us-central1-c"), "Zone or region for the cluster [env: CLUSTER_LOCATION]")
	poolCmd.PersistentFlags().StringVar(&cfg.NodePoolName, "pool", getEnv("NODE_POOL_NAME", "substrate-node-pool"), "Node pool name [env: NODE_POOL_NAME]")

	poolSetVersionLabelCmd.Flags().StringVar(&poolVersion, "version", "", "Substrate build version to set as the pool's node label")
	_ = poolSetVersionLabelCmd.MarkFlagRequired("version")
	poolSetVersionLabelCmd.Flags().BoolVar(&poolVersionIfAbsent, "only-if-absent", false, "Skip (successfully) when the pool already carries a different version label; for install-time stamping, where moving an existing label would flip every node at once")

	poolSetAutoscalingCmd.Flags().BoolVar(&poolAutoscalingOn, "enabled", false, "Whether autoscaling is enabled")
	poolSetAutoscalingCmd.Flags().Int32Var(&poolAutoscalingMin, "min", 0, "Minimum node count (with --enabled)")
	poolSetAutoscalingCmd.Flags().Int32Var(&poolAutoscalingMax, "max", 0, "Maximum node count (with --enabled)")
	poolSetAutoscalingCmd.Flags().StringVar(&poolAutoscalingJSON, "from-json", "", "Restore the exact NodePoolAutoscaling printed by get-autoscaling (overrides the other flags; preserves total_* limits and location policy)")

	poolCmd.AddCommand(poolSetVersionLabelCmd)
	poolCmd.AddCommand(poolGetAutoscalingCmd)
	poolCmd.AddCommand(poolSetAutoscalingCmd)
	rootCmd.AddCommand(poolCmd)
}

func poolResourceName(cfg *Config) string {
	return fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		cfg.ProjectID, cfg.ClusterLocation, cfg.ClusterName, cfg.NodePoolName)
}

func setPoolVersionLabel(ctx context.Context, cfg *Config, version string, onlyIfAbsent bool) error {
	if v := versionlabel.Value(version); v != version {
		return fmt.Errorf("version %q is not a valid label value (the controller would stamp %q)", version, v)
	}
	client, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	name := poolResourceName(cfg)
	pool, err := client.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: name})
	if err != nil {
		return fmt.Errorf("get node pool %s: %w", name, err)
	}

	labels, action := poolLabelPlan(pool.GetConfig().GetLabels(), version, onlyIfAbsent)
	switch action {
	case poolLabelNoop:
		slog.Info("Pool label already set", slog.String("pool", cfg.NodePoolName), slog.String("version", version))
		return nil
	case poolLabelSkip:
		slog.Info("Pool already carries a different version label; leaving it alone (--only-if-absent). Moving it belongs to the upgrade flow, after the roll.",
			slog.String("pool", cfg.NodePoolName), slog.String("have", pool.GetConfig().GetLabels()[versionlabel.Key]), slog.String("requested", version))
		return nil
	}

	slog.Info("Setting pool version label (applies in place to all existing nodes in the pool)",
		slog.String("pool", cfg.NodePoolName), slog.String("version", version))
	op, err := client.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{
		Name:   name,
		Labels: &containerpb.NodeLabels{Labels: labels},
	})
	if err != nil {
		return fmt.Errorf("update node pool labels: %w", err)
	}
	return waitContainerOperation(ctx, client, op.Name, cfg)
}

type poolLabelAction int

const (
	poolLabelSet poolLabelAction = iota
	poolLabelNoop
	poolLabelSkip
)

// poolLabelPlan returns the pool's full user label set with the version label
// set, and what to do with it. UpdateNodePool replaces the FULL set, so
// sending only the one key would strip every other label (e.g.
// ate.dev/sandboxClass). With onlyIfAbsent, an existing different version
// label is left alone: install-time stamping must never move a live fleet
// (the update applies in place to every node), that move is the upgrade
// flow's post-roll step.
func poolLabelPlan(existing map[string]string, version string, onlyIfAbsent bool) (map[string]string, poolLabelAction) {
	labels := map[string]string{}
	for k, v := range existing {
		// GetNodePool reports GKE-managed labels (e.g. sandbox.gke.io/runtime
		// on gVisor pools) alongside user labels, but UpdateNodePool rejects
		// any request naming a managed key. GKE keeps managing them whether
		// or not we send them; drop them from the set we write back.
		if !isManagedPoolLabelKey(k) {
			labels[k] = v
		}
	}
	current, has := labels[versionlabel.Key]
	if has && current == version {
		return labels, poolLabelNoop
	}
	if has && onlyIfAbsent {
		return labels, poolLabelSkip
	}
	labels[versionlabel.Key] = version
	return labels, poolLabelSet
}

// isManagedPoolLabelKey reports whether the label key belongs to GKE or
// Kubernetes rather than the user ("Node labels with key ... are managed by
// GKE or Kubernetes and must not be manually specified").
func isManagedPoolLabelKey(key string) bool {
	domain, _, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	for _, managed := range []string{"gke.io", "kubernetes.io", "k8s.io", "cloud.google.com"} {
		if domain == managed || strings.HasSuffix(domain, "."+managed) {
			return true
		}
	}
	return false
}

func printPoolAutoscaling(ctx context.Context, cfg *Config) error {
	client, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	pool, err := client.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: poolResourceName(cfg)})
	if err != nil {
		return fmt.Errorf("get node pool: %w", err)
	}
	a := pool.GetAutoscaling()
	if a == nil {
		a = &containerpb.NodePoolAutoscaling{}
	}
	b, err := protojson.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshal autoscaling: %w", err)
	}
	fmt.Printf("%s\n", b)
	return nil
}

func setPoolAutoscaling(ctx context.Context, cfg *Config, enabled bool, minNodes, maxNodes int32, fromJSON string) error {
	autoscaling := &containerpb.NodePoolAutoscaling{}
	if fromJSON != "" {
		if err := protojson.Unmarshal([]byte(fromJSON), autoscaling); err != nil {
			return fmt.Errorf("parse --from-json: %w", err)
		}
	} else {
		if enabled && maxNodes <= 0 {
			return fmt.Errorf("--enabled requires --max > 0")
		}
		autoscaling.Enabled = enabled
		if enabled {
			autoscaling.MinNodeCount = minNodes
			autoscaling.MaxNodeCount = maxNodes
		}
	}
	client, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	slog.Info("Setting pool autoscaling",
		slog.String("pool", cfg.NodePoolName), slog.String("config", autoscaling.String()))
	op, err := client.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{
		Name:        poolResourceName(cfg),
		Autoscaling: autoscaling,
	})
	if err != nil {
		return fmt.Errorf("set node pool autoscaling: %w", err)
	}
	return waitContainerOperation(ctx, client, op.Name, cfg)
}
