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
	"bytes"
	"strings"
	"testing"
)

func TestFilterAndDisplayLogLine(t *testing.T) {
	tests := []struct {
		name          string
		line          string
		targetActorID string
		raw           bool
		wantMatched   bool
		wantOutput    string
	}{
		{
			name:          "matching actor, pretty printed with RFC3339Nano",
			line:          `{"time":"2026-05-16T01:03:38.602878302Z","level":"info","msg":"Count","logging.googleapis.com/labels":{"ate.dev/actor_id":"act-1"}}`,
			targetActorID: "act-1",
			raw:           false,
			wantMatched:   true,
			wantOutput:    `[2026-05-16 01:03:38] [INFO] Count`,
		},
		{
			name:          "matching actor, pretty printed with message key",
			line:          `{"time":"2026-05-16T01:03:38Z","level":"warn","message":"Hello","logging.googleapis.com/labels":{"ate.dev/actor_id":"act-1"}}`,
			targetActorID: "act-1",
			raw:           false,
			wantMatched:   true,
			wantOutput:    `[2026-05-16 01:03:38] [WARN] Hello`,
		},
		{
			name:          "matching actor, pretty printed with no timestamp fallback",
			line:          `{"level":"error","msg":"Failed","logging.googleapis.com/labels":{"ate.dev/actor_id":"act-1"}}`,
			targetActorID: "act-1",
			raw:           false,
			wantMatched:   true,
			wantOutput:    `[ERROR] Failed`,
		},
		{
			name:          "matching actor, raw output requested",
			line:          `{"time":"2026-05-16T01:03:38.602878302Z","level":"info","msg":"Count","logging.googleapis.com/labels":{"ate.dev/actor_id":"act-1"}}`,
			targetActorID: "act-1",
			raw:           true,
			wantMatched:   true,
			wantOutput:    `{"time":"2026-05-16T01:03:38.602878302Z","level":"info","msg":"Count","logging.googleapis.com/labels":{"ate.dev/actor_id":"act-1"}}`,
		},
		{
			name:          "matching actor, fallback to standard labels key",
			line:          `{"time":"2026-05-16T01:03:38.602878302Z","level":"info","msg":"Count","labels":{"ate.dev/actor_id":"act-1"}}`,
			targetActorID: "act-1",
			raw:           false,
			wantMatched:   true,
			wantOutput:    `[2026-05-16 01:03:38] [INFO] Count`,
		},
		{
			name:          "non-matching actor",
			line:          `{"message":"Hello world","logging.googleapis.com/labels":{"ate.dev/actor_id":"act-2"}}`,
			targetActorID: "act-1",
			raw:           false,
			wantMatched:   false,
			wantOutput:    "",
		},
		{
			name:          "invalid json line",
			line:          "not a json line",
			targetActorID: "act-1",
			raw:           false,
			wantMatched:   false,
			wantOutput:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			matched := filterAndDisplayLogLine(tc.line, tc.targetActorID, &buf, tc.raw)

			if matched != tc.wantMatched {
				t.Errorf("got matched = %v, want %v", matched, tc.wantMatched)
			}

			gotOutput := strings.TrimSpace(buf.String())
			if gotOutput != tc.wantOutput {
				t.Errorf("got output %q, want %q", gotOutput, tc.wantOutput)
			}
		})
	}
}
