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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agent-substrate/substrate/internal/version"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
)

// setDefaultWorkerImageRepo stamps the ldflags-settable registry prefix for
// the duration of a test, mimicking a release build.
func setDefaultWorkerImageRepo(t *testing.T, repo string) {
	t.Helper()
	old := defaultWorkerImageRepo
	defaultWorkerImageRepo = repo
	t.Cleanup(func() { defaultWorkerImageRepo = old })
}

func TestDefaultWorkerImageNoRepo(t *testing.T) {
	setDefaultWorkerImageRepo(t, "")
	_, err := defaultWorkerImage(atev1alpha1.SandboxClassGvisor, "v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "no default worker image registry") {
		t.Fatalf("defaultWorkerImage() error = %v, want no-registry error", err)
	}
}

func TestDefaultWorkerImage(t *testing.T) {
	setDefaultWorkerImageRepo(t, "registry.test/substrate")
	tests := []struct {
		name         string
		class        atev1alpha1.SandboxClass
		buildVersion string
		want         string
		wantErr      string
	}{
		{
			name:         "gvisor",
			class:        atev1alpha1.SandboxClassGvisor,
			buildVersion: "v1.2.3",
			want:         defaultWorkerImageRepo + "/ateom-gvisor:v1.2.3",
		},
		{
			name:         "microvm",
			class:        atev1alpha1.SandboxClassMicroVM,
			buildVersion: "v1.2.3",
			want:         defaultWorkerImageRepo + "/ateom-microvm:v1.2.3",
		},
		{
			name:         "empty class defaults to gvisor",
			class:        "",
			buildVersion: "dev",
			want:         defaultWorkerImageRepo + "/ateom-gvisor:dev",
		},
		{
			name:         "git describe dirty output",
			class:        atev1alpha1.SandboxClassGvisor,
			buildVersion: "v0.1.0-3-gabc1234-dirty",
			want:         defaultWorkerImageRepo + "/ateom-gvisor:v0.1.0-3-gabc1234-dirty",
		},
		{
			name:         "semver build metadata sanitized",
			class:        atev1alpha1.SandboxClassGvisor,
			buildVersion: "1.2.3+build.4",
			want:         defaultWorkerImageRepo + "/ateom-gvisor:1.2.3-build.4",
		},
		{
			name:         "unknown class errors",
			class:        atev1alpha1.SandboxClass("firecracker"),
			buildVersion: "v1.2.3",
			wantErr:      `no default worker image for sandbox class "firecracker"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defaultWorkerImage(tt.class, tt.buildVersion)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("defaultWorkerImage() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("defaultWorkerImage() failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("defaultWorkerImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestImageTag(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"dev", "dev"},
		{"v1.2.3", "v1.2.3"},
		{"v0.1.0-3-gabc1234-dirty", "v0.1.0-3-gabc1234-dirty"},
		{"1.2.3+build", "1.2.3-build"},
		{"-leading-dash", "_leading-dash"},
		{"", "unknown"},
		{strings.Repeat("a", 200), strings.Repeat("a", 128)},
	}
	for _, tt := range tests {
		if got := imageTag(tt.in); got != tt.want {
			t.Errorf("imageTag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestBuildDeploymentApplyConfigWorkerImage covers the render-time resolution:
// an explicit spec.workerImage wins, an empty one gets the versioned default
// for the pool's sandbox class, and an unknown class is an explicit error.
func TestBuildDeploymentApplyConfigWorkerImage(t *testing.T) {
	setDefaultWorkerImageRepo(t, "registry.test/substrate")
	tests := []struct {
		name    string
		image   string
		class   atev1alpha1.SandboxClass
		want    string
		wantErr bool
	}{
		{
			name:  "explicit image wins",
			image: "example.com/custom-ateom:v9",
			class: atev1alpha1.SandboxClassMicroVM,
			want:  "example.com/custom-ateom:v9",
		},
		{
			name:  "default for gvisor",
			class: atev1alpha1.SandboxClassGvisor,
			want:  defaultWorkerImageRepo + "/ateom-gvisor:" + imageTag(version.Version),
		},
		{
			name:  "default for microvm",
			class: atev1alpha1.SandboxClassMicroVM,
			want:  defaultWorkerImageRepo + "/ateom-microvm:" + imageTag(version.Version),
		},
		{
			name: "default for empty class is gvisor",
			want: defaultWorkerImageRepo + "/ateom-gvisor:" + imageTag(version.Version),
		},
		{
			name:    "unknown class errors",
			class:   atev1alpha1.SandboxClass("bogus"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wp := &atev1alpha1.WorkerPool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "uid"},
				Spec: atev1alpha1.WorkerPoolSpec{
					Replicas:     1,
					WorkerImage:  tt.image,
					SandboxClass: tt.class,
				},
			}
			dep, err := buildDeploymentApplyConfig(wp, ateomOTelSettings{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("buildDeploymentApplyConfig() succeeded, want error for unknown sandbox class")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildDeploymentApplyConfig() failed: %v", err)
			}
			got := dep.Spec.Template.Spec.Containers[0].Image
			if got == nil || *got != tt.want {
				t.Errorf("worker image = %v, want %q", got, tt.want)
			}
		})
	}
}
