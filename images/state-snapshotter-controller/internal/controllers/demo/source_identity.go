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
)

// Demo snapshot Ready=False reasons for source identity resolution.
const (
	demoReasonInvalidSourceRef = "InvalidSourceRef"
	demoReasonSourceNotFound   = "SourceNotFound"
)

// demoSourceResolution is the outcome of resolving a demo snapshot's source identity from
// spec.sourceRef (the single source-of-truth). On failure Reason is non-empty and the caller sets
// Ready=False with Reason/Message.
type demoSourceResolution struct {
	Name    string
	Reason  string
	Message string
}

// resolveDemoSnapshotSource validates a demo snapshot's spec.sourceRef and returns the captured source
// object name. spec.sourceRef is required and immutable (enforced by the CRD), so it is the only
// identity this controller consults — both for manually-created snapshots and for root-planned ones.
func resolveDemoSnapshotSource(expectKind string, specRef demov1alpha1.SnapshotSourceRef) demoSourceResolution {
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
