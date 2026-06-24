/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package storagefoundation implements the storage-foundation VolumeCaptureRequest mechanics behind the
// SDK's data leg. The SDK talks to the foundation only through unstructured objects, so there is no Go
// dependency on the foundation API module.
package storagefoundation

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VolumeCaptureRequestGVK is the storage-foundation VolumeCaptureRequest GVK.
var VolumeCaptureRequestGVK = schema.GroupVersionKind{
	Group:   "storage.deckhouse.io",
	Version: "v1alpha1",
	Kind:    "VolumeCaptureRequest",
}

const volumeCaptureModeSnapshot = "Snapshot"

// Target identifies a PVC capture target (matches the foundation VolumeCaptureTarget JSON).
type Target struct {
	UID        string
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

// VCRName returns the deterministic data-leg VolumeCaptureRequest name owned by a snapshot, keyed by the
// snapshot UID. The name is derivable from the snapshot alone, without reading SnapshotContent.
func VCRName(snapshotUID types.UID) string {
	return fmt.Sprintf("snap-owned-vcr-%s", snapshotUID)
}

// Provider creates and reads storage-foundation VolumeCaptureRequests on behalf of the SDK data leg.
type Provider struct {
	client client.Client
}

// NewProvider returns a Provider backed by the given client.
func NewProvider(c client.Client) *Provider { return &Provider{client: c} }

// VCRName returns the deterministic VCR name for a snapshot UID.
func (p *Provider) VCRName(snapshotUID types.UID) string { return VCRName(snapshotUID) }

// EnsureVCR reconciles the snapshot's VolumeCaptureRequest toward the desired owner reference and targets.
// It creates the request when absent, adopts it (adds the owner reference) when present but unowned, and
// fails closed if an existing request's targets differ from the desired PVC target. The request is keyed by
// the snapshot UID, so the operation is idempotent and restart-safe.
func (p *Provider) EnsureVCR(ctx context.Context, namespace, name string, ownerRef metav1.OwnerReference, targets []Target) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(VolumeCaptureRequestGVK)
	err := p.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing)
	if apierrors.IsNotFound(err) {
		createErr := p.client.Create(ctx, newVolumeCaptureRequestObject(namespace, name, ownerRef, targets))
		if apierrors.IsAlreadyExists(createErr) {
			return p.EnsureVCR(ctx, namespace, name, ownerRef, targets)
		}
		return createErr
	}
	if err != nil {
		return err
	}

	if !hasOwnerRef(existing.GetOwnerReferences(), ownerRef) {
		base := existing.DeepCopy()
		existing.SetOwnerReferences(append(existing.GetOwnerReferences(), ownerRef))
		if patchErr := p.client.Patch(ctx, existing, client.MergeFrom(base)); patchErr != nil {
			return patchErr
		}
	}
	existingTargets, parseErr := parseTargets(existing)
	if parseErr != nil {
		return parseErr
	}
	if !targetsEqual(existingTargets, targets) {
		return fmt.Errorf("VolumeCaptureRequest %s/%s spec.targets differ from desired PVC target", namespace, name)
	}
	return nil
}

func hasOwnerRef(refs []metav1.OwnerReference, desired metav1.OwnerReference) bool {
	for _, ref := range refs {
		if ref.APIVersion == desired.APIVersion && ref.Kind == desired.Kind && ref.Name == desired.Name &&
			(desired.UID == "" || ref.UID == "" || ref.UID == desired.UID) {
			return true
		}
	}
	return false
}

func targetsEqual(a, b []Target) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]Target(nil), a...)
	bb := append([]Target(nil), b...)
	sortByUID(aa)
	sortByUID(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func sortByUID(ts []Target) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].UID < ts[j].UID })
}

// OwnedPVCTargets reads spec.targets[] from the snapshot's VolumeCaptureRequest. A missing request yields
// no targets (manifest-only or not yet created).
func (p *Provider) OwnedPVCTargets(ctx context.Context, namespace, vcrName string) ([]Target, error) {
	if vcrName == "" {
		return nil, nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(VolumeCaptureRequestGVK)
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vcrName}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseTargets(obj)
}

func newVolumeCaptureRequestObject(namespace, name string, ownerRef metav1.OwnerReference, targets []Target) *unstructured.Unstructured {
	specTargets := make([]interface{}, 0, len(targets))
	for _, t := range targets {
		specTargets = append(specTargets, map[string]interface{}{
			"uid":        t.UID,
			"apiVersion": t.APIVersion,
			"kind":       t.Kind,
			"name":       t.Name,
			"namespace":  t.Namespace,
		})
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": VolumeCaptureRequestGVK.Group + "/" + VolumeCaptureRequestGVK.Version,
		"kind":       VolumeCaptureRequestGVK.Kind,
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       namespace,
			"ownerReferences": []interface{}{ownerRefToMap(ownerRef)},
		},
		"spec": map[string]interface{}{
			"mode":    volumeCaptureModeSnapshot,
			"targets": specTargets,
		},
	}}
	obj.SetGroupVersionKind(VolumeCaptureRequestGVK)
	return obj
}

func ownerRefToMap(ref metav1.OwnerReference) map[string]interface{} {
	m := map[string]interface{}{
		"apiVersion": ref.APIVersion,
		"kind":       ref.Kind,
		"name":       ref.Name,
		"uid":        string(ref.UID),
	}
	if ref.Controller != nil {
		m["controller"] = *ref.Controller
	}
	return m
}

func parseTargets(obj *unstructured.Unstructured) ([]Target, error) {
	raw, found, err := unstructured.NestedSlice(obj.Object, "spec", "targets")
	if err != nil {
		return nil, err
	}
	if !found || len(raw) == 0 {
		return nil, nil
	}
	out := make([]Target, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("spec.targets[%d]: expected object", i)
		}
		out = append(out, Target{
			UID:        nestedString(m, "uid"),
			APIVersion: nestedString(m, "apiVersion"),
			Kind:       nestedString(m, "kind"),
			Name:       nestedString(m, "name"),
			Namespace:  nestedString(m, "namespace"),
		})
	}
	return out, nil
}

func nestedString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}
