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

// Command actor is the dual-stack POC counter actor with the upgrade drain
// contract: on SIGTERM (or POST /drain as delivery fallback) a cooperative
// actor finishes its simulated in-flight work item and then reports
// ready_for_suspend=true on /status while it KEEPS SERVING — its harness owns
// the suspend, the actor never exits. A stubborn actor (--cooperative=false)
// logs and ignores the notification, forcing the upgrade driver to kill it at
// the deadline. The counter lives only in RAM, so its continuity after a
// resume proves the snapshot round-trip.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	count           atomic.Uint64
	drainNotified   atomic.Bool
	workInFlight    atomic.Bool
	readyForSuspend atomic.Bool
	ready           atomic.Bool
)

func main() {
	cooperative := flag.Bool("cooperative", true, "handle drain notifications; false = ignore them (stubborn actor)")
	workInFlightDur := flag.Duration("work-in-flight", 2*time.Second, "how long the current work item takes to finish after a drain notification")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	drain := func(source string) {
		if !*cooperative {
			slog.Info("drain notification received - IGNORING (stubborn actor)", "source", source)
			return
		}
		if !drainNotified.CompareAndSwap(false, true) {
			return
		}
		workInFlight.Store(true)
		slog.Info("drain notification received - finishing in-flight work", "source", source)
		go func() {
			time.Sleep(*workInFlightDur)
			workInFlight.Store(false)
			readyForSuspend.Store(true)
			slog.Info("drain-ready: work finished, waiting for harness to suspend me", "count", count.Load())
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		for range sigCh {
			drain("SIGTERM")
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		slog.Info("handled request", "count", n)
		fmt.Fprintf(w, "count: %d | ready_for_suspend: %v | cooperative: %v\n",
			n, readyForSuspend.Load(), *cooperative)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"count":             count.Load(),
			"ready_for_suspend": readyForSuspend.Load(),
			"work_in_flight":    workInFlight.Load(),
		})
	})
	mux.HandleFunc("POST /drain", func(w http.ResponseWriter, _ *http.Request) {
		drain("http:/drain")
		fmt.Fprintf(w, "notified (cooperative=%v)\n", *cooperative)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	ready.Store(true)
	slog.Info("dualstack-poc actor starting", "cooperative", *cooperative)
	if err := http.ListenAndServe(":80", mux); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
