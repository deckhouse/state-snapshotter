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

package usecase

import storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"

// ResolveNamespaceSnapshotTargetNamespace is the single place that maps a NamespaceSnapshot
// object to the Kubernetes namespace being captured.
//
// NamespaceSnapshot is namespaced, so the resolved target namespace is metadata.namespace.
func ResolveNamespaceSnapshotTargetNamespace(nsSnap *storagev1alpha1.NamespaceSnapshot) string {
	if nsSnap == nil {
		return ""
	}
	return nsSnap.Namespace
}
