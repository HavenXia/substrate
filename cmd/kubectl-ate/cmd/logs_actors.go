//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/cmd/kubectl-ate/pkg/client"
	"github.com/agent-substrate/substrate/proto/ateapipb"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
)

var rawOutput bool

var logsActorsCmd = &cobra.Command{
	Use:     "actors <actor-id>",
	Aliases: []string{"actor"},
	Short:   "Stream logs for a specific actor",
	Args:    cobra.ExactArgs(1),
	RunE:    runLogsActor,
}

func init() {
	logsActorsCmd.Flags().BoolVar(&rawOutput, "raw", false, "Output raw JSON log lines instead of pretty-printed format")
	logsCmd.AddCommand(logsActorsCmd)
}

func runLogsActor(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	actorID := args[0]

	apiClient, err := client.NewClient(ctx, kubeconfig, k8sContext, endpoint, traceEnabled)
	if err != nil {
		return fmt.Errorf("failed to connect to ate-api-server: %w", err)
	}
	defer apiClient.Close()

	k8sClient, err := client.NewK8sClientset(kubeconfig, k8sContext)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	actorResp, err := apiClient.GetActor(ctx, &ateapipb.GetActorRequest{ActorId: actorID})
	if err != nil {
		return fmt.Errorf("failed to get actor: %w", err)
	}

	actor := actorResp.GetActor()
	podName := actor.GetAteomPodName()
	namespace := actor.GetAteomPodNamespace()

	if podName == "" || namespace == "" || actor.GetStatus() != ateapipb.Actor_STATUS_RUNNING {
		return fmt.Errorf("actor %s is not currently running on any worker pod", actorID)
	}

	opts := &corev1.PodLogOptions{
		Follow: false,
	}

	req := k8sClient.CoreV1().Pods(namespace).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("failed to stream logs from pod %s: %w", podName, err)
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		filterAndDisplayLogLine(scanner.Text(), actorID, os.Stdout, rawOutput)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading log stream: %w", err)
	}

	return nil
}

func filterAndDisplayLogLine(line, targetActorID string, w io.Writer, raw bool) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return false
	}
	labelsAny, ok := m["logging.googleapis.com/labels"]
	if !ok {
		labelsAny, ok = m["labels"]
		if !ok {
			return false
		}
	}
	labels, ok := labelsAny.(map[string]any)
	if !ok {
		return false
	}
	actorID, ok := labels["ate.dev/actor_id"].(string)
	if !ok || actorID != targetActorID {
		return false
	}

	if raw {
		fmt.Fprintln(w, line)
		return true
	}

	timeStr := ""
	if tVal, ok := m["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, tVal); err == nil {
			timeStr = t.Format("2006-01-02 15:04:05")
		} else if t, err := time.Parse(time.RFC3339, tVal); err == nil {
			timeStr = t.Format("2006-01-02 15:04:05")
		} else {
			timeStr = tVal
		}
	}

	levelStr := "INFO"
	if lVal, ok := m["level"].(string); ok {
		levelStr = strings.ToUpper(lVal)
	}

	msgStr := ""
	if mVal, ok := m["msg"].(string); ok {
		msgStr = mVal
	} else if mVal, ok := m["message"].(string); ok {
		msgStr = mVal
	}

	if timeStr != "" {
		fmt.Fprintf(w, "[%s] ", timeStr)
	}
	fmt.Fprintf(w, "[%s] %s\n", levelStr, msgStr)
	return true
}
