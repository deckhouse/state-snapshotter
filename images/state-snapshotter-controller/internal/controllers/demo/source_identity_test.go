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
	"encoding/json"
	"testing"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
)

func sourceRefAnnotation(t *testing.T, identity controllercommon.SnapshotSourceIdentity) map[string]string {
	t.Helper()
	b, err := json.Marshal(identity)
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}
	return map[string]string{controllercommon.AnnotationKeySourceRef: string(b)}
}

func validDiskIdentity() controllercommon.SnapshotSourceIdentity {
	return controllercommon.SnapshotSourceIdentity{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Namespace:  "ns1",
		Name:       "disk-a",
		UID:        "disk-uid",
	}
}

func TestResolveDemoSnapshotSource_AnnotationOneShotFill(t *testing.T) {
	res := resolveDemoSnapshotSource(sourceRefAnnotation(t, validDiskIdentity()), "ns1", controllercommon.KindDemoVirtualDisk, demov1alpha1.SnapshotSourceRef{})
	if res.Reason != "" {
		t.Fatalf("unexpected failure: %s/%s", res.Reason, res.Message)
	}
	if res.DeriveRef == nil {
		t.Fatalf("expected one-shot DeriveRef for empty spec.sourceRef")
	}
	want := demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       "disk-a",
	}
	if *res.DeriveRef != want {
		t.Fatalf("unexpected DeriveRef: %#v", *res.DeriveRef)
	}
	if res.Name != "disk-a" || res.UID != "disk-uid" {
		t.Fatalf("unexpected identity name/uid: %q/%q", res.Name, res.UID)
	}
}

func TestResolveDemoSnapshotSource_AnnotationMatchesExistingSpec(t *testing.T) {
	spec := demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       "disk-a",
	}
	res := resolveDemoSnapshotSource(sourceRefAnnotation(t, validDiskIdentity()), "ns1", controllercommon.KindDemoVirtualDisk, spec)
	if res.Reason != "" {
		t.Fatalf("unexpected failure: %s/%s", res.Reason, res.Message)
	}
	if res.DeriveRef != nil {
		t.Fatalf("must not re-derive when spec already matches")
	}
	if res.UID != "disk-uid" {
		t.Fatalf("expected uid from annotation, got %q", res.UID)
	}
}

func TestResolveDemoSnapshotSource_SpecImmutableOnDrift(t *testing.T) {
	spec := demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       "other-disk",
	}
	res := resolveDemoSnapshotSource(sourceRefAnnotation(t, validDiskIdentity()), "ns1", controllercommon.KindDemoVirtualDisk, spec)
	if res.Reason != demoReasonSourceRefImmutable {
		t.Fatalf("expected SourceRefImmutable, got %q (%s)", res.Reason, res.Message)
	}
}

func TestResolveDemoSnapshotSource_InvalidAnnotation(t *testing.T) {
	cases := map[string]map[string]string{
		"malformed json":  {controllercommon.AnnotationKeySourceRef: "{not-json"},
		"missing uid":     sourceRefAnnotation(t, controllercommon.SnapshotSourceIdentity{APIVersion: demov1alpha1.SchemeGroupVersion.String(), Kind: controllercommon.KindDemoVirtualDisk, Namespace: "ns1", Name: "disk-a"}),
		"wrong kind":      sourceRefAnnotation(t, controllercommon.SnapshotSourceIdentity{APIVersion: demov1alpha1.SchemeGroupVersion.String(), Kind: controllercommon.KindDemoVirtualMachine, Namespace: "ns1", Name: "disk-a", UID: "u"}),
		"wrong namespace": sourceRefAnnotation(t, controllercommon.SnapshotSourceIdentity{APIVersion: demov1alpha1.SchemeGroupVersion.String(), Kind: controllercommon.KindDemoVirtualDisk, Namespace: "other", Name: "disk-a", UID: "u"}),
	}
	for name, ann := range cases {
		t.Run(name, func(t *testing.T) {
			res := resolveDemoSnapshotSource(ann, "ns1", controllercommon.KindDemoVirtualDisk, demov1alpha1.SnapshotSourceRef{})
			if res.Reason != demoReasonInvalidSourceIdentityAnnotation {
				t.Fatalf("expected InvalidSourceIdentityAnnotation, got %q (%s)", res.Reason, res.Message)
			}
		})
	}
}

func TestResolveDemoSnapshotSource_ManualSpecFallback(t *testing.T) {
	spec := demov1alpha1.SnapshotSourceRef{
		APIVersion: demov1alpha1.SchemeGroupVersion.String(),
		Kind:       controllercommon.KindDemoVirtualDisk,
		Name:       "manual-disk",
	}
	res := resolveDemoSnapshotSource(nil, "ns1", controllercommon.KindDemoVirtualDisk, spec)
	if res.Reason != "" {
		t.Fatalf("unexpected failure: %s/%s", res.Reason, res.Message)
	}
	if res.Name != "manual-disk" || res.UID != "" {
		t.Fatalf("manual fallback must use spec name and no uid, got %q/%q", res.Name, res.UID)
	}
	if res.DeriveRef != nil {
		t.Fatalf("manual fallback must not derive spec")
	}
}

func TestResolveDemoSnapshotSource_ManualSpecInvalid(t *testing.T) {
	res := resolveDemoSnapshotSource(nil, "ns1", controllercommon.KindDemoVirtualDisk, demov1alpha1.SnapshotSourceRef{})
	if res.Reason != demoReasonInvalidSourceRef {
		t.Fatalf("expected InvalidSourceRef, got %q (%s)", res.Reason, res.Message)
	}
}
