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

package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
	vcctrl "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/volumecapture"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	vcpkg "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/volumecapture"
)

const defaultDemoSnapshotRequeueAfter = 500 * time.Millisecond

func demoSnapshotManifestCaptureRequestName(kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(kind + ":" + namespace + "/" + name))
	return "demo-mcr-" + hex.EncodeToString(sum[:10])
}

// ensureDemoSnapshotManifestCaptureRequest ensures the per-snapshot ManifestCaptureRequest (owned by the
// demo snapshot). Manifest targets are computed once at creation from the domain's own data-leg VCR (D3),
// never from SnapshotContent, so the domain controller stays content-free. The MCR is immutable after
// creation (dev clusters are recreated; no migration), so a later reconcile that no longer sees the VCR
// (deleted by the common controller after handoff) does not strip the captured PVC manifest target.
func ensureDemoSnapshotManifestCaptureRequest(
	ctx context.Context,
	c client.Client,
	namespace string,
	name string,
	kind string,
	targetAPIVersion string,
	targetKind string,
	targetName string,
	ownerRef metav1.OwnerReference,
	vcrName string,
) (*ssv1alpha1.ManifestCaptureRequest, error) {
	mcrName := demoSnapshotManifestCaptureRequestName(kind, namespace, name)
	key := types.NamespacedName{Namespace: namespace, Name: mcrName}
	existing := &ssv1alpha1.ManifestCaptureRequest{}
	err := c.Get(ctx, key, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	desiredTargets, err := demoManifestCaptureTargetsFromVCR(ctx, c, namespace, vcrName, []ssv1alpha1.ManifestTarget{{
		APIVersion: targetAPIVersion,
		Kind:       targetKind,
		Name:       targetName,
	}})
	if err != nil {
		return nil, err
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:            mcrName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: ssv1alpha1.ManifestCaptureRequestSpec{
			Targets: desiredTargets,
		},
	}
	if err := c.Create(ctx, mcr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ensureDemoSnapshotManifestCaptureRequest(ctx, c, namespace, name, kind, targetAPIVersion, targetKind, targetName, ownerRef, vcrName)
		}
		return nil, err
	}
	return mcr, nil
}

// demoManifestCaptureTargetsFromVCR appends the owned-PVC manifest targets derived from the domain's own
// data-leg VCR spec.targets to base. This replaces deriving owned PVC targets from
// SnapshotContent.status.dataRefs (D3): the result is identical (the covered PVC manifest is included),
// but it relies only on the domain's own VCR, never on SnapshotContent.
func demoManifestCaptureTargetsFromVCR(
	ctx context.Context,
	c client.Reader,
	namespace string,
	vcrName string,
	base []ssv1alpha1.ManifestTarget,
) ([]ssv1alpha1.ManifestTarget, error) {
	nmBase := make([]namespacemanifest.ManifestTarget, 0, len(base))
	for _, t := range base {
		nmBase = append(nmBase, namespacemanifest.ManifestTarget{
			APIVersion: t.APIVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}
	merged := nmBase
	if vcrName != "" {
		vcr := &unstructured.Unstructured{}
		vcr.SetGroupVersionKind(vcpkg.VolumeCaptureRequestGVK)
		getErr := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vcrName}, vcr)
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return nil, getErr
		}
		if getErr == nil {
			targets, parseErr := vcctrl.ParseVolumeCaptureTargets(vcr)
			if parseErr != nil {
				return nil, parseErr
			}
			owned := namespacemanifest.ManifestTargetsFromVolumeTargets(targets)
			merged = namespacemanifest.AppendOwnedPVCManifestTargets(nmBase, owned, nil, namespace)
		}
	}
	out := make([]ssv1alpha1.ManifestTarget, 0, len(merged))
	for _, t := range merged {
		out = append(out, ssv1alpha1.ManifestTarget{
			APIVersion: t.APIVersion,
			Kind:       t.Kind,
			Name:       t.Name,
		})
	}
	return out, nil
}

func demoSnapshotOwnerReference(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        uid,
		Controller: &controller,
	}
}

func demoSnapshotOwnerRefMatches(ref, desired metav1.OwnerReference) bool {
	if ref.APIVersion != desired.APIVersion || ref.Kind != desired.Kind || ref.Name != desired.Name {
		return false
	}
	return desired.UID == "" || ref.UID == "" || ref.UID == desired.UID
}

func ensureDemoSnapshotOwnerRef(obj client.Object, desired metav1.OwnerReference) error {
	refs := make([]metav1.OwnerReference, 0, len(obj.GetOwnerReferences())+1)
	desiredSet := false
	for _, ref := range obj.GetOwnerReferences() {
		if demoSnapshotOwnerRefMatches(ref, desired) {
			if !desiredSet {
				refs = append(refs, desired)
				desiredSet = true
			}
			continue
		}
		if isSnapshotParentOwnerRef(ref) {
			return fmt.Errorf("child snapshot %s/%s is already owned by %s/%s", obj.GetNamespace(), obj.GetName(), ref.Kind, ref.Name)
		}
		if ref.Controller != nil && *ref.Controller {
			return fmt.Errorf("child snapshot %s/%s already has controller ownerRef %s/%s", obj.GetNamespace(), obj.GetName(), ref.Kind, ref.Name)
		}
		refs = append(refs, ref)
	}
	if !desiredSet {
		refs = append(refs, desired)
	}
	if !controllercommon.OwnerReferencesEqual(obj.GetOwnerReferences(), refs) {
		obj.SetOwnerReferences(refs)
	}
	return nil
}

func isSnapshotParentOwnerRef(ref metav1.OwnerReference) bool {
	if ref.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && ref.Kind == controllercommon.KindSnapshot {
		return true
	}
	if ref.APIVersion == demov1alpha1.SchemeGroupVersion.String() && ref.Kind == controllercommon.KindDemoVirtualMachineSnapshot {
		return true
	}
	return false
}
