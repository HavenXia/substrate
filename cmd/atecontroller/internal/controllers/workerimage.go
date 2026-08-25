// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"fmt"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
)

// defaultWorkerImageRepo is the registry prefix for the default worker images
// injected when spec.workerImage is unset. Empty unless a release build that
// actually publishes ateom images stamps it via
// -ldflags "-X <module>/cmd/atecontroller/internal/controllers.defaultWorkerImageRepo=<registry>".
//
// While empty, pools omitting spec.workerImage fail reconcile with an
// actionable error rather than rendering an image reference nothing can pull.
var defaultWorkerImageRepo string

var defaultWorkerImageNames = map[atev1alpha1.SandboxClass]string{
	atev1alpha1.SandboxClassGvisor:  "ateom-gvisor",
	atev1alpha1.SandboxClassMicroVM: "ateom-microvm",
}

// resolveWorkerImage returns the worker image for a pool: spec.workerImage
// when set, otherwise the compiled-in default for the pool's sandbox class
// tagged with buildVersion.
func resolveWorkerImage(wp *atev1alpha1.WorkerPool, buildVersion string) (string, error) {
	if wp.Spec.WorkerImage != "" {
		return wp.Spec.WorkerImage, nil
	}
	return defaultWorkerImage(wp.Spec.SandboxClass, buildVersion)
}

func defaultWorkerImage(class atev1alpha1.SandboxClass, buildVersion string) (string, error) {
	if defaultWorkerImageRepo == "" {
		return "", fmt.Errorf("this build carries no default worker image registry; set spec.workerImage explicitly")
	}
	if class == "" {
		class = atev1alpha1.SandboxClassGvisor
	}
	name, ok := defaultWorkerImageNames[class]
	// Guard for explicitly failing old controller + new CRD with a new SandboxClass
	if !ok {
		return "", fmt.Errorf("no default worker image for sandbox class %q", class)
	}
	return fmt.Sprintf("%s/%s:%s", defaultWorkerImageRepo, name, imageTag(buildVersion)), nil
}

// imageTag normalizes a build version into a valid image tag
// ([a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}). The version is free-form via -ldflags,
// so invalid bytes are mapped deterministically instead of rejected.
func imageTag(v string) string {
	if v == "" {
		return "unknown"
	}
	b := []byte(v)
	for i := range b {
		c := b[i]
		switch {
		case c == '_' || '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z':
		case i > 0 && (c == '.' || c == '-'):
		case i == 0:
			b[i] = '_'
		default:
			b[i] = '-'
		}
	}
	if len(b) > 128 {
		b = b[:128]
	}
	return string(b)
}
