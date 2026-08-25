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
	"github.com/spf13/cobra"
)

var upgradeRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Roll nodes back to a previous substrate version",
	Long: `Runs the upgrade loop in reverse: nodes are processed in the opposite order
and relabeled to the previous version. The old worker Deployments were never
deleted (see "upgrade cleanup"), so their Pending pods reseat as each node's
label flips back.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUpgradeRoll(cmd, true)
	},
}

func init() {
	addUpgradeRollFlags(upgradeRollbackCmd)
	upgradeCmd.AddCommand(upgradeRollbackCmd)
}
