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

package domain_rbac

import (
	"context"
	"fmt"

	"github.com/deckhouse/module-sdk/pkg"
	sdkk8s "github.com/deckhouse/module-sdk/pkg/dependency/k8s"
	"github.com/deckhouse/module-sdk/pkg/registry"
	"github.com/deckhouse/module-sdk/pkg/utils/ptr"
	"github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = registry.RegisterFunc(
	&pkg.HookConfig{
		Kubernetes: []pkg.KubernetesConfig{{
			Name:                         "custom-snapshot-definitions",
			APIVersion:                   "state-snapshotter.deckhouse.io/v1alpha1",
			Kind:                         "CustomSnapshotDefinition",
			ExecuteHookOnSynchronization: ptr.Bool(true),
			ExecuteHookOnEvents:          ptr.Bool(true),
		}},
		Queue: "modules/" + consts.ModuleName,
	},
	reconcileDomainRBAC,
)

// reconcileDomainRBAC is the main reconcile function. On every CSD change it:
//  1. Lists all CSDs and selects those with Accepted=True at current generation.
//  2. Resolves source and snapshot GVKs → GVRs for each eligible CSD.
//  3. Creates/updates the split ClusterRoles + bindings: the DOMAIN SA gets source/snapshot GVR rights
//     (incl. /status, /finalizers) + get on core's per-CR /manifests-download; the CORE SA gets read +
//     create + patch + status-write on the snapshot GVRs (it is the parent-graph planner: creates and
//     ownerRef-patches one child snapshot per source), get + list on the source GVRs (list to enumerate
//     sources during planning, get to capture each target's manifest), and get on the domain
//     /manifests-with-data-restoration subresource.
//  4. Writes SourceAccessGranted (True / Pending / ApplyFailed) on each eligible CSD.
func reconcileDomainRBAC(ctx context.Context, input *pkg.HookInput) error {
	cl := input.DC.MustGetK8sClient(sdkk8s.WithSchemeBuilder(v1alpha1.SchemeBuilder))

	// list all CSDs
	list := new(v1alpha1.CustomSnapshotDefinitionList)
	if err := cl.List(ctx, list); err != nil {
		return fmt.Errorf("cannot list CSDs: %w", err)
	}

	// filter with Accepted=True
	eligible := filterAcceptedCSD(list.Items)

	resolver := restMapperResolver(cl.RESTMapper())
	sourceGVRs, snapshotGVRs, pendingByName := resolveEligibleGVRs(eligible, resolver)

	// DOMAIN SA: dynamic source/snapshot GVR rights + get on core's per-CR /manifests-download subresource.
	domainRules := buildRules(sourceGVRs, snapshotGVRs)
	domainRules = append(domainRules, coreManifestsSubresourceRules(snapshotGVRs)...)

	// CORE SA: read + create + patch + status-write on the dynamic demo snapshot GVRs (the core
	// SnapshotReconciler is the parent-graph planner: it creates one child snapshot per source and patches
	// its ownerRef), get + list on the demo source GVRs (list to enumerate sources during planning, get for
	// per-target manifest capture), and get on the domain /manifests-with-data-restoration subresource
	// (restore delegation).
	coreReadRules := buildCoreReadRules(snapshotGVRs)
	coreReadRules = append(coreReadRules, buildCoreSourceReadRules(sourceGVRs)...)
	coreReadRules = append(coreReadRules, domainRestoreSubresourceRules(snapshotGVRs)...)

	// DataExport (storage-foundation) SA: read-only on the dynamic demo snapshot GVRs so the
	// DataExport controller can GET the snapshot leaf (status.boundSnapshotContentName) when exporting.
	dataExportReadRules := buildDataExportReadRules(snapshotGVRs)

	applyErr := applyDomainRBAC(ctx, cl, domainRules, coreReadRules, dataExportReadRules)

	for i := range eligible {
		csd := &eligible[i]
		var cond metav1.Condition
		switch {
		case pendingByName[csd.Name] != "":
			cond = desiredSourceAccessGrantedCondition(csd.Generation,
				metav1.ConditionFalse, consts.SourceAccessGrantedReasonPending,
				pendingByName[csd.Name])
		case applyErr != nil:
			cond = desiredSourceAccessGrantedCondition(csd.Generation,
				metav1.ConditionFalse, consts.SourceAccessGrantedReasonApplyFailed,
				applyErr.Error())
		default:
			cond = desiredSourceAccessGrantedCondition(csd.Generation,
				metav1.ConditionTrue, consts.SourceAccessGrantedReasonApplied,
				"domain RBAC applied for all source and snapshot GVRs")
		}
		if err := patchCSDSourceAccessGranted(ctx, cl, csd.Name, cond); err != nil {
			input.Logger.Error("patch SourceAccessGranted on CSD", "name", csd.Name, "err", err)
		}
	}

	return applyErr
}
