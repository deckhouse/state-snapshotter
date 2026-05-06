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

package namespacemanifest

import (
	"context"
	"testing"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestBuildManifestCaptureTargets_EmptyNamespaceHasNoTargets(t *testing.T) {
	listKinds := make(map[schema.GroupVersionResource]string, len(n2aNamespacedGVR))
	for _, gvr := range n2aNamespacedGVR {
		listKinds[gvr] = gvr.Resource + "List"
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), listKinds)

	targets, err := BuildManifestCaptureTargets(context.Background(), dyn, "ns1")
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets in empty namespace, got %#v", targets)
	}
}
