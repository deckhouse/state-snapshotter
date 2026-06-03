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
	"fmt"

	demov1alpha1 "github.com/deckhouse/state-snapshotter/api/demo/v1alpha1"
	controllercommon "github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/controllers/common"
)

// Demo snapshot Ready=False reasons for source identity resolution.
//
// These reasons describe whether the demo domain controller could accept the
// snapshot. Generic tree correctness does NOT depend on spec.sourceRef: the
// only generic source-of-truth is the framework annotation
// state-snapshotter.deckhouse.io/source-ref. spec.sourceRef is a demo/manual
// API-compat field that this controller MAY derive from the annotation.
const (
	demoReasonInvalidSourceRef                = "InvalidSourceRef"
	demoReasonInvalidSourceIdentityAnnotation = "InvalidSourceIdentityAnnotation"
	demoReasonSourceRefImmutable              = "SourceRefImmutable"
	demoReasonSourceNotFound                  = "SourceNotFound"
	demoReasonSourceUIDMismatch               = "SourceUIDMismatch"
)

// demoSourceResolution is the outcome of resolving a demo snapshot's source
// identity. On failure Reason is non-empty and the caller sets Ready=False with
// Reason/Message. When DeriveRef is non-nil the caller must one-shot fill
// spec.sourceRef from it (it was empty) and requeue.
type demoSourceResolution struct {
	Name      string
	UID       string
	DeriveRef *demov1alpha1.SnapshotSourceRef
	Reason    string
	Message   string
}

// resolveDemoSnapshotSource resolves a demo snapshot's source identity.
//
// Precedence:
//   - the generic framework annotation (state-snapshotter.deckhouse.io/source-ref)
//     is authoritative when present; the controller derives its demo spec.sourceRef
//     from it (one-shot) and treats the derived value as immutable afterwards;
//   - spec.sourceRef is the only identity for manually-created demo snapshots
//     that carry no annotation.
func resolveDemoSnapshotSource(annotations map[string]string, namespace, expectKind string, specRef demov1alpha1.SnapshotSourceRef) demoSourceResolution {
	if annotations[controllercommon.AnnotationKeySourceRef] != "" {
		identity, err := controllercommon.DecodeSnapshotSourceIdentityAnnotations(annotations)
		if err != nil {
			return demoSourceResolution{Reason: demoReasonInvalidSourceIdentityAnnotation, Message: err.Error()}
		}
		if identity.APIVersion != demov1alpha1.SchemeGroupVersion.String() {
			return demoSourceResolution{Reason: demoReasonInvalidSourceIdentityAnnotation, Message: fmt.Sprintf("%s apiVersion must be %q", controllercommon.AnnotationKeySourceRef, demov1alpha1.SchemeGroupVersion.String())}
		}
		if identity.Kind != expectKind {
			return demoSourceResolution{Reason: demoReasonInvalidSourceIdentityAnnotation, Message: fmt.Sprintf("%s kind must be %q", controllercommon.AnnotationKeySourceRef, expectKind)}
		}
		if identity.Namespace != namespace {
			return demoSourceResolution{Reason: demoReasonInvalidSourceIdentityAnnotation, Message: fmt.Sprintf("%s namespace must be %q", controllercommon.AnnotationKeySourceRef, namespace)}
		}
		derived := demov1alpha1.SnapshotSourceRef{
			APIVersion: identity.APIVersion,
			Kind:       identity.Kind,
			Name:       identity.Name,
		}
		if demoSourceRefEmpty(specRef) {
			return demoSourceResolution{Name: identity.Name, UID: identity.UID, DeriveRef: &derived}
		}
		if specRef != derived {
			return demoSourceResolution{Reason: demoReasonSourceRefImmutable, Message: fmt.Sprintf("spec.sourceRef is immutable once set; must remain %s/%s", derived.Kind, derived.Name)}
		}
		return demoSourceResolution{Name: identity.Name, UID: identity.UID}
	}

	// Manual demo snapshot: spec.sourceRef is the only identity available.
	if specRef.APIVersion != demov1alpha1.SchemeGroupVersion.String() {
		return demoSourceResolution{Reason: demoReasonInvalidSourceRef, Message: fmt.Sprintf("spec.sourceRef.apiVersion must be %q", demov1alpha1.SchemeGroupVersion.String())}
	}
	if specRef.Kind != expectKind {
		return demoSourceResolution{Reason: demoReasonInvalidSourceRef, Message: fmt.Sprintf("spec.sourceRef.kind must be %q", expectKind)}
	}
	if specRef.Name == "" {
		return demoSourceResolution{Reason: demoReasonInvalidSourceRef, Message: "spec.sourceRef.name is required"}
	}
	return demoSourceResolution{Name: specRef.Name}
}

func demoSourceRefEmpty(ref demov1alpha1.SnapshotSourceRef) bool {
	return ref.APIVersion == "" && ref.Kind == "" && ref.Name == ""
}
