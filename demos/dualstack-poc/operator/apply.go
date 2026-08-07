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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

const fieldOwner = "substrate-operator"

// applyBundle server-side-applies every document of a multi-doc yaml file.
// Idempotent; re-run every pass in the phases that own the bundle.
func (r *reconciler) applyBundle(ctx context.Context, dir, file string) error {
	if dir == "" {
		return fmt.Errorf("no bundle dir: set spec.bundleDir or --bundle-dir")
	}
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return err
	}
	// Fresh mapper per apply: earlier docs may create the CRDs later docs use
	// (a stale-discovery miss is retried by the next pass).
	groups, err := restmapper.GetAPIGroupResources(r.k8s.Discovery())
	if err != nil {
		return fmt.Errorf("discovering API groups: %w", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groups)

	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(doc, &obj.Object); err != nil {
			return fmt.Errorf("%s: parsing document: %w", file, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("%s: mapping %s %s: %w", file, gvk, obj.GetName(), err)
		}
		ri := r.dyn.Resource(mapping.Resource)
		var target dynamic.ResourceInterface = ri
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			if obj.GetNamespace() == "" {
				return fmt.Errorf("%s: namespaced %s %q has no namespace", file, gvk.Kind, obj.GetName())
			}
			target = ri.Namespace(obj.GetNamespace())
		}
		if _, err := target.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: fieldOwner, Force: true}); err != nil {
			return fmt.Errorf("%s: applying %s %s/%s: %w", file, gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
	}
}

// ---- rollout readiness ----

// stackBlockers reports the per-stack objects for a suffix that are not yet
// rolled out. NotFound counts as a blocker (level-triggered wait).
func (r *reconciler) stackBlockers(ctx context.Context, suffix string) ([]string, error) {
	var blockers []string
	for _, name := range []string{"ate-api-server-" + suffix, "ate-controller-" + suffix, "atenet-router-" + suffix, "dns-" + suffix} {
		d, err := r.k8s.AppsV1().Deployments(ateSystemNS).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			blockers = append(blockers, "deployment/"+name+" (not found)")
		case err != nil:
			return nil, err
		case !deploymentReady(d):
			blockers = append(blockers, "deployment/"+name)
		}
	}
	ds, err := r.k8s.AppsV1().DaemonSets(ateSystemNS).Get(ctx, "atelet-"+suffix, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		blockers = append(blockers, "daemonset/atelet-"+suffix+" (not found)")
	case err != nil:
		return nil, err
	case !daemonSetReady(ds):
		blockers = append(blockers, "daemonset/atelet-"+suffix)
	}
	return blockers, nil
}

func (r *reconciler) sharedBlockers(ctx context.Context) ([]string, error) {
	var blockers []string
	d, err := r.k8s.AppsV1().Deployments(ateSystemNS).Get(ctx, "ate-dispatcher", metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		blockers = append(blockers, "deployment/ate-dispatcher (not found)")
	case err != nil:
		return nil, err
	case !deploymentReady(d):
		blockers = append(blockers, "deployment/ate-dispatcher")
	}
	sts, err := r.k8s.AppsV1().StatefulSets(ateSystemNS).Get(ctx, "valkey-cluster", metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		blockers = append(blockers, "statefulset/valkey-cluster (not found)")
	case err != nil:
		return nil, err
	case !statefulSetReady(sts):
		blockers = append(blockers, "statefulset/valkey-cluster")
	}
	return blockers, nil
}

func deploymentReady(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas >= want && d.Status.ReadyReplicas >= want
}

func daemonSetReady(ds *appsv1.DaemonSet) bool {
	// Desired 0 means no labeled nodes exist yet; that is not "ready".
	return ds.Status.ObservedGeneration >= ds.Generation &&
		ds.Status.DesiredNumberScheduled > 0 &&
		ds.Status.UpdatedNumberScheduled >= ds.Status.DesiredNumberScheduled &&
		ds.Status.NumberReady >= ds.Status.DesiredNumberScheduled
}

func statefulSetReady(sts *appsv1.StatefulSet) bool {
	want := int32(1)
	if sts.Spec.Replicas != nil {
		want = *sts.Spec.Replicas
	}
	return sts.Status.ObservedGeneration >= sts.Generation && sts.Status.ReadyReplicas >= want
}

// ---- flip ----

func (r *reconciler) greenRoutersReady(ctx context.Context, version string) (ready, total int, err error) {
	pods, err := r.k8s.CoreV1().Pods(ateSystemNS).List(ctx, metav1.ListOptions{
		LabelSelector: "app=atenet-router,substrate-version=" + version,
	})
	if err != nil {
		return 0, 0, err
	}
	for i := range pods.Items {
		if podReady(&pods.Items[i]) {
			ready++
		}
	}
	return ready, len(pods.Items), nil
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// flipRouterDoor repoints the shared atenet-router door Service at the new
// stack's router pods.
func (r *reconciler) flipRouterDoor(ctx context.Context, version string) error {
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"selector": map[string]string{"app": "atenet-router", "substrate-version": version},
		},
	})
	if err != nil {
		return err
	}
	_, err = r.k8s.CoreV1().Services(ateSystemNS).Patch(ctx, "atenet-router", types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (r *reconciler) flipDispatcherRules(ctx context.Context) error {
	patch, err := json.Marshal(map[string]any{
		"data": map[string]string{"rules.json": `{"mode":"upgrade"}`},
	})
	if err != nil {
		return err
	}
	_, err = r.k8s.CoreV1().ConfigMaps(ateSystemNS).Patch(ctx, "ate-dispatcher-rules", types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// repointTemplates flips every ActorTemplate workerSelector pinned to the old
// version. Requires the CEL immutability rule relaxed on the shared CRD.
func (r *reconciler) repointTemplates(ctx context.Context, old, newV string) (int, error) {
	list, err := r.crd.ApiV1alpha1().ActorTemplates(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	flipped := 0
	for i := range list.Items {
		t := &list.Items[i]
		sel := t.Spec.WorkerSelector
		if sel == nil || sel.MatchLabels["substrate-version"] != old {
			continue
		}
		sel.MatchLabels["substrate-version"] = newV
		if _, err := r.crd.ApiV1alpha1().ActorTemplates(t.Namespace).Update(ctx, t, metav1.UpdateOptions{}); err != nil {
			return flipped, fmt.Errorf("updating %s/%s: %w", t.Namespace, t.Name, err)
		}
		flipped++
	}
	return flipped, nil
}

// ---- migrate / teardown ----

// deleteWorkerPods force-deletes each straggler's worker pod (grace 0); the
// old stack's syncer then crashes the actor and deletes the worker row.
func (r *reconciler) deleteWorkerPods(ctx context.Context, actors []*ateapipb.Actor) error {
	grace := int64(0)
	for _, a := range actors {
		as := a.GetWorkerAssignment()
		if as.GetWorkerPod() == "" {
			continue
		}
		err := r.k8s.CoreV1().Pods(as.GetWorkerNamespace()).Delete(ctx, as.GetWorkerPod(), metav1.DeleteOptions{GracePeriodSeconds: &grace})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting worker pod %s/%s: %w", as.GetWorkerNamespace(), as.GetWorkerPod(), err)
		}
	}
	return nil
}

var teardownGVRs = []schema.GroupVersionResource{
	{Group: "apps", Version: "v1", Resource: "deployments"},
	{Group: "apps", Version: "v1", Resource: "daemonsets"},
	{Group: "", Version: "v1", Resource: "services"},
	{Group: "", Version: "v1", Resource: "configmaps"},
	{Group: "ate.dev", Version: "v1alpha1", Resource: "workerpools"},
	{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"},
}

// teardownStack deletes every labeled per-stack object of the old version.
// Shared objects are unlabeled and survive; the dispatcher rules ConfigMap
// stays because it carries no substrate-version label.
func (r *reconciler) teardownStack(ctx context.Context, version string) error {
	opts := metav1.ListOptions{LabelSelector: "substrate-version=" + version}
	for _, ns := range []string{ateSystemNS, demoNS} {
		for _, gvr := range teardownGVRs {
			ri := r.dyn.Resource(gvr).Namespace(ns)
			err := ri.DeleteCollection(ctx, metav1.DeleteOptions{}, opts)
			switch {
			case err == nil || apierrors.IsNotFound(err):
			case apierrors.IsMethodNotSupported(err):
				// Older API servers lack deletecollection for Services.
				list, lerr := ri.List(ctx, opts)
				if lerr != nil {
					return lerr
				}
				for i := range list.Items {
					if derr := ri.Delete(ctx, list.Items[i].GetName(), metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
						return derr
					}
				}
			default:
				return fmt.Errorf("deleting %s in %s: %w", gvr.Resource, ns, err)
			}
		}
	}
	return nil
}
