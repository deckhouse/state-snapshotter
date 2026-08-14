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

package tests

// The «scenario x field» completeness contract of SnapshotContent.status.data (api .../dataleg) applied to
// content published by a real controller in a real cluster. The unit-level drive proves each path carries
// what its fixtures attest; only a cluster proves it against a real CSI driver, a real DataImport and a real
// checkpoint-rebuilt sourceRef.
//
// The contract itself — which field each of the four paths must publish, and why an empty one is or is not
// legitimate — lives in the api module and is shared with the controller tests. It is deliberately NOT
// restated here: a second copy is a copy that stops matching.

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/api/storage/v1alpha1/dataleg"
)

// snapshotContentDataCompleteness judges a published SnapshotContent.status.data against the completeness
// matrix, resolving the cell from the cluster itself: the content's spec.snapshotRef says whether the owner
// is a CSI VolumeSnapshot, and the owner's spec.mode says whether this is an import. Those are the same two
// structural discriminators the controller routes on, and dataleg.Classify maps them to the cell — so the
// assertion cannot end up judging an import leg against the capture column.
//
// hasData=false means the content publishes no status.data at all (a manifest-only node): nothing was
// judged, and the caller must report that rather than count it as a pass.
func snapshotContentDataCompleteness(ctx context.Context, contentName string) (res dataleg.Result, hasData bool, err error) {
	content, err := getResource(ctx, snapshotContentGVR, "", contentName)
	if err != nil {
		return dataleg.Result{}, false, fmt.Errorf("get SnapshotContent %s: %w", contentName, err)
	}

	scenario, err := snapshotContentScenario(ctx, content)
	if err != nil {
		return dataleg.Result{}, false, err
	}

	data, found, err := unstructured.NestedMap(content.Object, "status", "data")
	if err != nil {
		return dataleg.Result{}, false, fmt.Errorf("read status.data of SnapshotContent %s: %w", contentName, err)
	}
	if !found || len(data) == 0 {
		return dataleg.Result{}, false, nil
	}
	var binding storagev1alpha1.SnapshotDataBinding
	if cerr := runtime.DefaultUnstructuredConverter.FromUnstructured(data, &binding); cerr != nil {
		return dataleg.Result{}, false, fmt.Errorf("decode status.data of SnapshotContent %s into SnapshotDataBinding: %w", contentName, cerr)
	}

	res, err = dataleg.Check(scenario, &binding)
	if err != nil {
		return dataleg.Result{}, true, fmt.Errorf("SnapshotContent %s: %w", contentName, err)
	}
	return res, true, nil
}

// snapshotContentScenario resolves which of the four data-leg paths published this content, by reading the
// owner it is bound to. A missing owner is an error rather than a guess: the cell decides what the content is
// required to carry, so getting it wrong would quietly judge the wrong column.
func snapshotContentScenario(ctx context.Context, content *unstructured.Unstructured) (dataleg.Scenario, error) {
	kind, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "kind")
	ns, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "namespace")
	name, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "name")
	if kind == "" || name == "" {
		return "", fmt.Errorf("SnapshotContent %s has no spec.snapshotRef kind/name, so its data-leg scenario cannot be resolved", content.GetName())
	}
	gvr, ok := gvrForSnapshotKind(kind)
	if !ok {
		return "", fmt.Errorf("SnapshotContent %s is owned by unknown snapshot kind %q", content.GetName(), kind)
	}
	owner, err := getResource(ctx, gvr, ns, name)
	if err != nil {
		return "", fmt.Errorf("get owner %s %s/%s of SnapshotContent %s: %w", kind, ns, name, content.GetName(), err)
	}
	mode, _, _ := unstructured.NestedString(owner.Object, "spec", "mode")
	// "VolumeSnapshot" is the native-CSI leg discriminator (the same kind check reconcileDataLegProjection
	// makes); the suite spells this kind literally everywhere, the controller constant is in a module e2e
	// does not import.
	return dataleg.Classify(kind == "VolumeSnapshot", mode == string(storagev1alpha1.SnapshotModeImport)), nil
}

// assertSnapshotContentDataComplete is the error-returning form for the flows that already report failures
// that way (the import round trip). It REQUIRES a published status.data: it is called where the leg is
// already known to be published, and an absent one there is the wedge this contract exists to catch.
func assertSnapshotContentDataComplete(ctx context.Context, contentName, what string) error {
	res, hasData, err := snapshotContentDataCompleteness(ctx, contentName)
	if err != nil {
		return err
	}
	if !hasData {
		return fmt.Errorf("%s: SnapshotContent %s published no status.data at all", what, contentName)
	}
	if !res.OK() {
		return fmt.Errorf("%s: %s", what, res.Report())
	}
	// A settled leg must leave nothing undecidable: an undecided cell is a field NOT judged, and it stays
	// invisible here unless it is rejected — today the only conditional cell (fsType) is undecidable solely
	// when volumeMode is empty, which is a violation of its own, but that coupling is a property of the
	// current table, not of this assertion. Same requirement as the other two consumers of the matrix.
	if len(res.Skipped) != 0 {
		return fmt.Errorf("%s: a settled data leg must leave no cell undecidable: %s", what, res.Report())
	}
	if res.Checked == 0 {
		return fmt.Errorf("%s: SnapshotContent %s was judged against zero field expectations", what, contentName)
	}
	GinkgoWriter.Printf("  %s: %s\n", what, res.Report())
	return nil
}

// assertLeafSnapshotContentDataComplete resolves a snapshot leaf's bound SnapshotContent and judges it.
func assertLeafSnapshotContentDataComplete(ctx context.Context, ns, kind, leafName string) error {
	gvr, ok := gvrForSnapshotKind(kind)
	if !ok {
		return fmt.Errorf("assertLeafSnapshotContentDataComplete: unknown snapshot kind %q (%s)", kind, leafName)
	}
	leaf, err := getResource(ctx, gvr, ns, leafName)
	if err != nil {
		return fmt.Errorf("get %s %s/%s: %w", kind, ns, leafName, err)
	}
	contentName, _, _ := unstructured.NestedString(leaf.Object, "status", "boundSnapshotContentName")
	if contentName == "" {
		return fmt.Errorf("%s %s/%s is bound to no SnapshotContent", kind, ns, leafName)
	}
	return assertSnapshotContentDataComplete(ctx, contentName, fmt.Sprintf("data leg of %s %s/%s", kind, ns, leafName))
}
