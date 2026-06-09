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
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// isImportedDemoObject reports whether a demo snapshot object carries the imported annotation
// set by the snapshot import controller.  When true, live source validation, MCR creation,
// and lifecycle setup must be skipped; the reconciler should only mirror the pre-published
// content Ready status via demoMirrorOnlyIfHandoffComplete.
func isImportedDemoObject(obj client.Object) bool {
	if obj == nil {
		return false
	}
	v, ok := obj.GetAnnotations()[ssv1alpha1.AnnotationImported]
	return ok && v == "true"
}
