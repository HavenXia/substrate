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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"
)

const (
	modePassthrough = "passthrough"
	modeUpgrade     = "upgrade"
)

// rulesWatcher polls a rules file ({"mode":"passthrough"} or
// {"mode":"upgrade"}) and exposes the current mode. Unreadable or invalid
// content keeps the last good mode; the initial mode is passthrough.
type rulesWatcher struct {
	path string
	mode atomic.Value // string

	// Stat of the file at the last successful load; only touched by the
	// polling goroutine (and once before it starts).
	modTime time.Time
	size    int64
}

func startRules(path string) *rulesWatcher {
	w := &rulesWatcher{path: path}
	w.mode.Store(modePassthrough)
	if err := w.load(); err != nil {
		log.Printf("rules: initial load failed, defaulting to %s: %v", modePassthrough, err)
	}
	go func() {
		for range time.Tick(2 * time.Second) {
			if err := w.load(); err != nil {
				log.Printf("rules: reload failed, keeping %s: %v", w.Mode(), err)
			}
		}
	}()
	return w
}

func (w *rulesWatcher) Mode() string {
	return w.mode.Load().(string)
}

// load re-stats the file and re-reads it only when it changed since the last
// successful load (ConfigMap updates land as a new file behind a symlink
// swap, so identity or mtime always moves).
func (w *rulesWatcher) load() error {
	fi, err := os.Stat(w.path)
	if err != nil {
		return err
	}
	if fi.ModTime().Equal(w.modTime) && fi.Size() == w.size {
		return nil
	}
	raw, err := os.ReadFile(w.path)
	if err != nil {
		return err
	}
	var r struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("parse %s: %w", w.path, err)
	}
	if r.Mode != modePassthrough && r.Mode != modeUpgrade {
		return fmt.Errorf("unknown mode %q in %s", r.Mode, w.path)
	}
	w.modTime, w.size = fi.ModTime(), fi.Size()
	if prev := w.Mode(); prev != r.Mode {
		log.Printf("rules: mode %s -> %s", prev, r.Mode)
	}
	w.mode.Store(r.Mode)
	return nil
}
