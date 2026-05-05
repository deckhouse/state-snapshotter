/*
Copyright 2025 Flant JSC

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

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
)

func rootObjectKeeperName(snapshotNamespace, snapshotAPIVersion, snapshotKind, snapshotName string) string {
	if snapshotAPIVersion == storagev1alpha1.SchemeGroupVersion.String() && snapshotKind == KindNamespaceSnapshot {
		return namespacemanifest.NamespaceSnapshotRootObjectKeeperName(snapshotNamespace, snapshotName)
	}
	sum := sha256.Sum256([]byte(snapshotAPIVersion + "|" + snapshotKind + "|" + snapshotNamespace + "/" + snapshotName))
	return "ret-snap-" + hex.EncodeToString(sum[:10])
}

func rootObjectKeeperTTL(cfg *config.Options) time.Duration {
	if cfg != nil && cfg.SnapshotRootOKTTL > 0 {
		return cfg.SnapshotRootOKTTL
	}
	return config.DefaultSnapshotRootOKTTL
}

func ensureRootObjectKeeperWithTTL(
	ctx context.Context,
	c client.Client,
	apiReader client.Reader,
	cfg *config.Options,
	snapshotObj client.Object,
	snapshotGVK schema.GroupVersionKind,
) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	if snapshotObj.GetUID() == "" {
		return nil, ctrl.Result{Requeue: true}, nil
	}
	name := rootObjectKeeperName(snapshotObj.GetNamespace(), snapshotGVK.GroupVersion().String(), snapshotGVK.Kind, snapshotObj.GetName())
	want := deckhousev1alpha1.ObjectKeeperSpec{
		Mode: ObjectKeeperModeFollowObjectWithTTL,
		FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
			APIVersion: snapshotGVK.GroupVersion().String(),
			Kind:       snapshotGVK.Kind,
			Namespace:  snapshotObj.GetNamespace(),
			Name:       snapshotObj.GetName(),
			UID:        string(snapshotObj.GetUID()),
		},
		TTL: &metav1.Duration{Duration: rootObjectKeeperTTL(cfg)},
	}

	ok := &deckhousev1alpha1.ObjectKeeper{}
	err := c.Get(ctx, client.ObjectKey{Name: name}, ok)
	if err == nil {
		if err := validateRootObjectKeeper(name, ok, want); err != nil {
			return nil, ctrl.Result{}, err
		}
		if !rootObjectKeeperSpecMatches(want, ok) {
			base := ok.DeepCopy()
			ok.Spec = want
			if err := c.Patch(ctx, ok, client.MergeFrom(base)); err != nil {
				return nil, ctrl.Result{}, err
			}
			if apiReader != nil {
				if err := apiReader.Get(ctx, client.ObjectKey{Name: name}, ok); err != nil {
					return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
				}
			}
		}
		return ok, ctrl.Result{}, nil
	}
	if client.IgnoreNotFound(err) != nil {
		return nil, ctrl.Result{}, err
	}

	ok = &deckhousev1alpha1.ObjectKeeper{
		TypeMeta: metav1.TypeMeta{
			APIVersion: DeckhouseAPIVersion,
			Kind:       KindObjectKeeper,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       want,
	}
	if err := c.Create(ctx, ok); err != nil {
		return nil, ctrl.Result{}, err
	}
	reader := apiReader
	if reader == nil {
		reader = c
	}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, ok); err != nil {
		return nil, ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}
	return ok, ctrl.Result{}, nil
}

func validateRootObjectKeeper(name string, ok *deckhousev1alpha1.ObjectKeeper, want deckhousev1alpha1.ObjectKeeperSpec) error {
	if ok.Spec.FollowObjectRef == nil || want.FollowObjectRef == nil {
		return fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", name)
	}
	gotRef := ok.Spec.FollowObjectRef
	wantRef := want.FollowObjectRef
	if gotRef.APIVersion != wantRef.APIVersion ||
		gotRef.Kind != wantRef.Kind ||
		gotRef.Namespace != wantRef.Namespace ||
		gotRef.Name != wantRef.Name ||
		gotRef.UID != wantRef.UID {
		return fmt.Errorf("ObjectKeeper %s belongs to another snapshot-run", name)
	}
	return nil
}

func rootObjectKeeperSpecMatches(want deckhousev1alpha1.ObjectKeeperSpec, got *deckhousev1alpha1.ObjectKeeper) bool {
	if got.Spec.Mode != want.Mode {
		return false
	}
	if got.Spec.FollowObjectRef == nil || want.FollowObjectRef == nil {
		return false
	}
	gotRef := got.Spec.FollowObjectRef
	wantRef := want.FollowObjectRef
	if gotRef.APIVersion != wantRef.APIVersion ||
		gotRef.Kind != wantRef.Kind ||
		gotRef.Namespace != wantRef.Namespace ||
		gotRef.Name != wantRef.Name ||
		gotRef.UID != wantRef.UID {
		return false
	}
	if got.Spec.TTL == nil || want.TTL == nil {
		return got.Spec.TTL == nil && want.TTL == nil
	}
	return got.Spec.TTL.Duration == want.TTL.Duration
}

func rootObjectKeeperOwnerReference(ok *deckhousev1alpha1.ObjectKeeper) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: DeckhouseAPIVersion,
		Kind:       KindObjectKeeper,
		Name:       ok.Name,
		UID:        ok.UID,
		Controller: &controller,
	}
}

func snapshotContentOwnerReference(content *storagev1alpha1.SnapshotContent) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       "SnapshotContent",
		Name:       content.Name,
		UID:        content.UID,
		Controller: &controller,
	}
}

func ensureLifecycleOwnerRef(ctx context.Context, c client.Client, obj client.Object, desired metav1.OwnerReference) (bool, error) {
	refs, changed, err := lifecycleOwnerRefs(obj.GetOwnerReferences(), desired)
	if err != nil {
		return false, fmt.Errorf("%s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
	}
	if !changed {
		return false, nil
	}
	base := obj.DeepCopyObject().(client.Object)
	obj.SetOwnerReferences(refs)
	return true, c.Patch(ctx, obj, client.MergeFrom(base))
}

func lifecycleOwnerRefs(existing []metav1.OwnerReference, desired metav1.OwnerReference) ([]metav1.OwnerReference, bool, error) {
	out := make([]metav1.OwnerReference, 0, len(existing)+1)
	desiredSet := false
	for _, ref := range existing {
		if isLifecycleOwnerRef(ref) {
			if ownerRefSameIdentity(ref, desired) {
				if !desiredSet {
					out = append(out, desired)
					desiredSet = true
				}
				continue
			}
			return nil, false, fmt.Errorf("conflicting lifecycle ownerRef %s/%s", ref.Kind, ref.Name)
		}
		out = append(out, ref)
	}
	if !desiredSet {
		out = append(out, desired)
	}
	return out, !ownerReferencesEqual(existing, out), nil
}

func isLifecycleOwnerRef(ref metav1.OwnerReference) bool {
	if ref.Kind == KindObjectKeeper || ref.Kind == "SnapshotContent" {
		return true
	}
	return strings.HasSuffix(ref.Kind, "Snapshot")
}

func ownerRefSameIdentity(ref, desired metav1.OwnerReference) bool {
	return ref.APIVersion == desired.APIVersion &&
		ref.Kind == desired.Kind &&
		ref.Name == desired.Name &&
		(ref.UID == "" || desired.UID == "" || ref.UID == desired.UID)
}

func snapshotParentOwnerRef(obj client.Object) *metav1.OwnerReference {
	for _, ref := range obj.GetOwnerReferences() {
		if strings.HasSuffix(ref.Kind, "Snapshot") {
			refCopy := ref
			return &refCopy
		}
	}
	return nil
}

func resolveParentSnapshotContentOwnerRef(ctx context.Context, c client.Client, child client.Object) (*metav1.OwnerReference, bool, error) {
	parentRef := snapshotParentOwnerRef(child)
	if parentRef == nil {
		return nil, false, nil
	}

	var parentContentName string
	switch {
	case parentRef.APIVersion == storagev1alpha1.SchemeGroupVersion.String() && parentRef.Kind == KindNamespaceSnapshot:
		parent := &storagev1alpha1.NamespaceSnapshot{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: child.GetNamespace(), Name: parentRef.Name}, parent); err != nil {
			return nil, false, err
		}
		if parentRef.UID != "" && parent.UID != parentRef.UID {
			return nil, false, fmt.Errorf("parent NamespaceSnapshot %s/%s UID mismatch", child.GetNamespace(), parentRef.Name)
		}
		parentContentName = parent.Status.BoundSnapshotContentName
	case parentRef.APIVersion == demov1alpha1.SchemeGroupVersion.String() && parentRef.Kind == KindDemoVirtualMachineSnapshot:
		parent := &demov1alpha1.DemoVirtualMachineSnapshot{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: child.GetNamespace(), Name: parentRef.Name}, parent); err != nil {
			return nil, false, err
		}
		if parentRef.UID != "" && parent.UID != parentRef.UID {
			return nil, false, fmt.Errorf("parent DemoVirtualMachineSnapshot %s/%s UID mismatch", child.GetNamespace(), parentRef.Name)
		}
		parentContentName = parent.Status.BoundSnapshotContentName
	default:
		gv, err := schema.ParseGroupVersion(parentRef.APIVersion)
		if err != nil {
			return nil, false, err
		}
		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(gv.WithKind(parentRef.Kind))
		if err := c.Get(ctx, client.ObjectKey{Namespace: child.GetNamespace(), Name: parentRef.Name}, parent); err != nil {
			return nil, false, err
		}
		if parentRef.UID != "" && parent.GetUID() != parentRef.UID {
			return nil, false, fmt.Errorf("parent %s %s/%s UID mismatch", parentRef.Kind, child.GetNamespace(), parentRef.Name)
		}
		name, _, err := unstructured.NestedString(parent.Object, "status", "boundSnapshotContentName")
		if err != nil {
			return nil, false, err
		}
		parentContentName = name
	}
	if parentContentName == "" {
		return nil, true, nil
	}
	parentContent := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: parentContentName}, parentContent); err != nil {
		return nil, false, err
	}
	ref := snapshotContentOwnerReference(parentContent)
	return &ref, false, nil
}
