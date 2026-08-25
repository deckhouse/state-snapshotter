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

package transform

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func dataSourceRefName(pvc unstructured.Unstructured) string {
	name, _, _ := unstructured.NestedString(pvc.Object, "spec", "dataSourceRef", "name")
	return name
}

func TestCompileNodeManifests_OrphanPVCGetsDataSourceRef(t *testing.T) {
	pvc := pvcManifest("data-pvc", "source-ns", "uid-a")

	out, err := CompileNodeManifests(NodeRestoreInput{
		CapturedManifests:           []unstructured.Unstructured{pvc},
		NodeNamespace:               "restore-ns",
		PVCDataBindings:             []PVCDataBinding{dataBindingRef("uid-a", "data-pvc", "vsc-a")},
		VolumeSnapshotByContentName: map[string]string{"vsc-a": "vs-a"},
	})
	if err != nil {
		t.Fatalf("CompileNodeManifests: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 object, got %d", len(out))
	}
	if out[0].GetNamespace() != "restore-ns" {
		t.Fatalf("namespace = %q, want restore-ns", out[0].GetNamespace())
	}
	if got := dataSourceRefName(out[0]); got != "vs-a" {
		t.Fatalf("dataSourceRef.name = %q, want vs-a", got)
	}
	apiGroup, _, _ := unstructured.NestedString(out[0].Object, "spec", "dataSourceRef", "apiGroup")
	if apiGroup != csiSnapshotAPIGroup {
		t.Fatalf("dataSourceRef.apiGroup = %q, want %q", apiGroup, csiSnapshotAPIGroup)
	}
}

func TestCompileNodeManifests_OrphanPVCMissingVSLeafFailsClosed(t *testing.T) {
	pvc := pvcManifest("data-pvc", "source-ns", "uid-a")

	_, err := CompileNodeManifests(NodeRestoreInput{
		CapturedManifests:           []unstructured.Unstructured{pvc},
		NodeNamespace:               "restore-ns",
		PVCDataBindings:             []PVCDataBinding{dataBindingRef("uid-a", "data-pvc", "vsc-a")},
		VolumeSnapshotByContentName: map[string]string{}, // VS leaf not resolved
	})
	if err == nil {
		t.Fatal("expected contract violation when VS leaf is missing")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

func TestCompileNodeManifests_GenericPassThrough(t *testing.T) {
	cm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "cfg", "namespace": "source-ns"},
		"data":       map[string]interface{}{"k": "v"},
	}}

	out, err := CompileNodeManifests(NodeRestoreInput{
		CapturedManifests: []unstructured.Unstructured{cm},
		NodeNamespace:     "restore-ns",
	})
	if err != nil {
		t.Fatalf("CompileNodeManifests: %v", err)
	}
	if len(out) != 1 || out[0].GetKind() != "ConfigMap" {
		t.Fatalf("expected ConfigMap pass-through, got %#v", out)
	}
	if out[0].GetNamespace() != "restore-ns" {
		t.Fatalf("namespace = %q, want restore-ns", out[0].GetNamespace())
	}
}

func TestCompileNodeManifests_PVCWithoutDataRefFailsClosed(t *testing.T) {
	pvc := pvcManifest("plain-pvc", "source-ns", "uid-plain")

	_, err := CompileNodeManifests(NodeRestoreInput{
		CapturedManifests:           []unstructured.Unstructured{pvc},
		NodeNamespace:               "restore-ns",
		VolumeSnapshotByContentName: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error: a PVC without a data binding must not be emitted data-less")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
}

// TestCompileNodeManifests_OrphanPVCNonVSCArtifactFailsClosed pins the artifact-kind rule on its own: the
// binding resolves to a name that IS published as a VolumeSnapshot, so nothing else would stop the
// compilation — only the artifact kind is wrong. Restoring from such a binding would point the PVC at
// something that is not a durable volume snapshot, so it is refused and named.
func TestCompileNodeManifests_OrphanPVCNonVSCArtifactFailsClosed(t *testing.T) {
	pvc := pvcManifest("data-pvc", "source-ns", "uid-a")
	binding := dataBindingRef("uid-a", "data-pvc", "artifact-x")
	binding.Artifact.Kind = "ConfigMap" // not a VolumeSnapshotContent

	_, err := CompileNodeManifests(NodeRestoreInput{
		CapturedManifests:           []unstructured.Unstructured{pvc},
		NodeNamespace:               "restore-ns",
		PVCDataBindings:             []PVCDataBinding{binding},
		VolumeSnapshotByContentName: map[string]string{"artifact-x": "vs-x"},
	})
	if err == nil {
		t.Fatal("expected contract violation when orphan-PVC artifact is not a VolumeSnapshotContent")
	}
	if !errors.Is(err, ErrContractViolation) {
		t.Fatalf("expected ErrContractViolation, got %v", err)
	}
	if !strings.Contains(err.Error(), `data artifact is "ConfigMap"`) {
		t.Fatalf("refusal must name the wrong artifact kind, got %q", err.Error())
	}
}

// TestCompileNodeManifests_DomainCoveredPVCIsDropped pins the one domain-supplied input: a PVC the domain
// object recreates on restore is neither emitted nor required to have a data binding. Dropping it is what
// makes the fail-closed rule usable by a domain — without it every domain node with a disk would be
// refused — and emitting it would restore a second, empty copy of the same volume alongside the one the
// domain object brings.
func TestCompileNodeManifests_DomainCoveredPVCIsDropped(t *testing.T) {
	covered := pvcManifest("disk-pvc", "source-ns", "uid-covered")
	cm := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "cfg", "namespace": "source-ns"},
	}}

	out, err := CompileNodeManifests(NodeRestoreInput{
		CapturedManifests:     []unstructured.Unstructured{covered, cm},
		NodeNamespace:         "restore-ns",
		DomainCoveredPVCNames: map[string]struct{}{"disk-pvc": {}},
	})
	if err != nil {
		t.Fatalf("CompileNodeManifests: %v", err)
	}
	if len(out) != 1 || out[0].GetKind() != "ConfigMap" {
		t.Fatalf("expected only the ConfigMap, got %#v", out)
	}
}

// NodeRestoreInput.SourceNamespace exists for a caller whose manifests no longer carry the captured
// namespace — a facade that attaches the target namespace to namespace-relative manifests before
// compiling. The whole value of the field is that ONE line of plumbing, and without a test at this level
// losing it is invisible: both in-repo consumers leave the field empty and rely on the inference, so the
// sanitize-level guards stay green while the field silently stops reaching the sanitizer.
//
// The fixture is built so inference CANNOT produce the right answer: metadata.namespace is already the
// target, so a compiler that drops SourceNamespace sees source == target, recognises nothing as moved,
// and leaves the subject pointing at a namespace that no longer holds the account.
func TestCompileNodeManifests_SourceNamespaceReachesTheSanitizer(t *testing.T) {
	const (
		sourceNS = "captured-ns"
		targetNS = "restore-ns"
	)
	if sourceNS == targetNS {
		t.Fatalf("fixture is vacuous: source and target must differ (source=%s target=%s)", sourceNS, targetNS)
	}

	rb := unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		// Already the target: this is what a facade hands in, and what makes inference useless here.
		"metadata": map[string]interface{}{"name": "rb", "namespace": targetNS},
		"roleRef":  map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "r"},
		"subjects": []interface{}{
			map[string]interface{}{"kind": "ServiceAccount", "name": "app", "namespace": sourceNS},
		},
	}}

	out, err := CompileNodeManifests(NodeRestoreInput{
		CapturedManifests: []unstructured.Unstructured{rb},
		SourceNamespace:   sourceNS,
		NodeNamespace:     targetNS,
	})
	if err != nil {
		t.Fatalf("CompileNodeManifests: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected exactly the RoleBinding back, got %d objects", len(out))
	}

	subjects, found, err := unstructured.NestedSlice(out[0].Object, "subjects")
	if err != nil {
		t.Fatalf("reading subjects: %v", err)
	}
	if !found || len(subjects) != 1 {
		t.Fatalf("expected exactly one subject, got found=%v %#v", found, subjects)
	}
	subject, ok := subjects[0].(map[string]interface{})
	if !ok {
		t.Fatalf("subject must be an object, got %#v", subjects[0])
	}
	if subject["namespace"] != targetNS {
		t.Fatalf("subject namespace = %v, want %s — SourceNamespace did not reach the sanitizer",
			subject["namespace"], targetNS)
	}
	t.Logf("subjects checked: %d (source %s recognised as moved to %s)", len(subjects), sourceNS, targetNS)
}
