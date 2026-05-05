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
	"fmt"
	"strings"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// DSC condition types (ADR: snapshot-rework/2026-01-23-unified-snapshots-registry.md).
const (
	DSCConditionAccepted  = "Accepted"
	DSCConditionRBACReady = "RBACReady"
	DSCConditionReady     = "Ready"
)

const (
	DSCReasonKindConflict = "KindConflict"
	DSCReasonInvalidSpec  = "InvalidSpec"
	// DSCReadyReasonNotReady is the Ready condition reason when the aggregate is false (not a spec-level standardized reason).
	DSCReadyReasonNotReady = "NotReady"
)

// DomainSpecificSnapshotControllerReconciler resolves snapshotResourceMapping, detects cross-DSC
// snapshot kind conflicts, writes Accepted and aggregated Ready. RBACReady is owned by Deckhouse hook.
// Runtime watch activation is triggered after successful status reconciliation.
//
// Phase-1 trade-off: Reconcile ignores the triggering request and always List()s all DSCs, then fully
// recomputes resolution and conflicts for every object. Any update to one DSC re-runs the whole cycle.
// This is intentional for correctness and simplicity; optimize later if needed.
type DomainSpecificSnapshotControllerReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
	Logger logger.LoggerInterface
	Config *config.Options

	// UnifiedRuntimeSync runs after a successful full DSC reconcile. Production wiring always provides it;
	// nil remains valid for focused unit tests.
	UnifiedRuntimeSync func(context.Context) error

	// GraphRegistryRefresh rebuilds the generic Snapshot graph GVK registry. Production wiring
	// always provides it; nil remains valid for focused unit tests.
	GraphRegistryRefresh func(context.Context) error
}

func NewDomainSpecificSnapshotControllerReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	log logger.LoggerInterface,
	cfg *config.Options,
) (*DomainSpecificSnapshotControllerReconciler, error) {
	if c == nil {
		return nil, fmt.Errorf("client must not be nil")
	}
	if scheme == nil {
		return nil, fmt.Errorf("scheme must not be nil")
	}
	if log == nil {
		return nil, fmt.Errorf("logger must not be nil")
	}
	return &DomainSpecificSnapshotControllerReconciler{
		Client: c,
		Scheme: scheme,
		Logger: log,
		Config: cfg,
	}, nil
}

func (r *DomainSpecificSnapshotControllerReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	var list storagev1alpha1.DomainSpecificSnapshotControllerList
	if err := r.Client.List(ctx, &list); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileAll(ctx, list.Items); err != nil {
		r.Logger.Error(err, "DSC reconcileAll failed")
		return ctrl.Result{}, err
	}
	if r.GraphRegistryRefresh != nil {
		if err := r.GraphRegistryRefresh(ctx); err != nil {
			r.Logger.Warning("snapshot graph registry refresh after DSC reconcile failed", "error", err)
		}
	}
	if r.UnifiedRuntimeSync != nil {
		if err := r.UnifiedRuntimeSync(ctx); err != nil {
			r.Logger.Warning("unified GVK runtime sync after DSC reconcile failed", "error", err)
		}
	}
	return ctrl.Result{}, nil
}

type dscEntryResolution struct {
	perMapping []mappingResolution
	// duplicateSnapshotKind is true if two mappings in the same DSC resolve to the same snapshot GroupKind.
	duplicateSnapshotKind bool
}

type mappingResolution struct {
	snapshotGK schema.GroupKind
	resolveErr error
}

func (r *DomainSpecificSnapshotControllerReconciler) reconcileAll(ctx context.Context, items []storagev1alpha1.DomainSpecificSnapshotController) error {
	resByName, conflicting := r.computeDSCGlobalStateFromItems(ctx, items)
	for i := range items {
		d := &items[i]
		if err := r.writeStatusIfNeeded(ctx, d, resByName, conflicting); err != nil {
			return fmt.Errorf("update status for DSC %q: %w", d.Name, err)
		}
	}
	return nil
}

// computeDSCGlobalStateFromItems resolves every DSC spec and derives KindConflict participants.
func (r *DomainSpecificSnapshotControllerReconciler) computeDSCGlobalStateFromItems(
	ctx context.Context,
	items []storagev1alpha1.DomainSpecificSnapshotController,
) (resByName map[string]dscEntryResolution, conflicting map[string]struct{}) {
	resByName = make(map[string]dscEntryResolution, len(items))
	for i := range items {
		d := &items[i]
		resByName[d.Name] = r.resolveDSCSpec(ctx, d)
	}

	snapshotOwners := make(map[string]map[string]struct{})
	for i := range items {
		d := &items[i]
		res := resByName[d.Name]
		if res.duplicateSnapshotKind {
			continue
		}
		hasErr := false
		for _, m := range res.perMapping {
			if m.resolveErr != nil {
				hasErr = true
				break
			}
		}
		if hasErr {
			continue
		}
		seenInDSC := make(map[string]struct{})
		for _, m := range res.perMapping {
			gkKey := m.snapshotGK.String()
			if _, dup := seenInDSC[gkKey]; dup {
				hasErr = true
				break
			}
			seenInDSC[gkKey] = struct{}{}
		}
		if hasErr {
			continue
		}
		for gk := range seenInDSC {
			if snapshotOwners[gk] == nil {
				snapshotOwners[gk] = make(map[string]struct{})
			}
			snapshotOwners[gk][d.Name] = struct{}{}
		}
	}

	conflicting = make(map[string]struct{})
	for _, owners := range snapshotOwners {
		if len(owners) > 1 {
			for name := range owners {
				conflicting[name] = struct{}{}
			}
		}
	}
	return resByName, conflicting
}

func (r *DomainSpecificSnapshotControllerReconciler) resolveDSCSpec(ctx context.Context, d *storagev1alpha1.DomainSpecificSnapshotController) dscEntryResolution {
	var out dscEntryResolution
	out.perMapping = make([]mappingResolution, 0, len(d.Spec.SnapshotResourceMapping))
	seenGK := make(map[string]struct{})

	for _, entry := range d.Spec.SnapshotResourceMapping {
		mr := mappingResolution{}
		snapCRD, err := r.getCRD(ctx, entry.SnapshotCRDName)
		if err != nil {
			mr.resolveErr = fmt.Errorf("snapshot CRD %q: %w", entry.SnapshotCRDName, err)
			out.perMapping = append(out.perMapping, mr)
			continue
		}
		_, err = r.getCRD(ctx, entry.ResourceCRDName)
		if err != nil {
			mr.resolveErr = fmt.Errorf("resource CRD %q: %w", entry.ResourceCRDName, err)
			out.perMapping = append(out.perMapping, mr)
			continue
		}
		snapGVK, err := gvkFromCRD(snapCRD)
		if err != nil {
			mr.resolveErr = fmt.Errorf("snapshot GVK: %w", err)
			out.perMapping = append(out.perMapping, mr)
			continue
		}
		mr.snapshotGK = snapGVK.GroupKind()

		if _, dup := seenGK[mr.snapshotGK.String()]; dup {
			out.duplicateSnapshotKind = true
		}
		seenGK[mr.snapshotGK.String()] = struct{}{}

		out.perMapping = append(out.perMapping, mr)
	}
	return out
}

func (r *DomainSpecificSnapshotControllerReconciler) getCRD(ctx context.Context, crdName string) (*extv1.CustomResourceDefinition, error) {
	crd := &extv1.CustomResourceDefinition{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: crdName}, crd); err != nil {
		return nil, err
	}
	return crd, nil
}

func gvkFromCRD(crd *extv1.CustomResourceDefinition) (schema.GroupVersionKind, error) {
	ver := storedVersion(crd)
	if ver == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("CRD %q has no storage version", crd.Name)
	}
	return schema.GroupVersionKind{
		Group:   crd.Spec.Group,
		Version: ver,
		Kind:    crd.Spec.Names.Kind,
	}, nil
}

func storedVersion(crd *extv1.CustomResourceDefinition) string {
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			return crd.Spec.Versions[i].Name
		}
	}
	return ""
}

func (r *DomainSpecificSnapshotControllerReconciler) writeStatusIfNeeded(
	ctx context.Context,
	d *storagev1alpha1.DomainSpecificSnapshotController,
	resByName map[string]dscEntryResolution,
	conflicting map[string]struct{},
) error {
	res := resByName[d.Name]
	gen := d.GetGeneration()

	acceptedStatus, acceptedReason, acceptedMsg := r.computeAccepted(d.Name, res, conflicting)
	rbac := meta.FindStatusCondition(d.Status.Conditions, DSCConditionRBACReady)
	ready := computeDSCReady(acceptedStatus, rbac, gen)

	desired := buildDSCStatusConditions(gen, acceptedStatus, acceptedReason, acceptedMsg, ready, d.Status.Conditions)

	if dscConditionsSemanticallyEqual(d.Status.Conditions, desired) {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var list storagev1alpha1.DomainSpecificSnapshotControllerList
		if err := r.Client.List(ctx, &list); err != nil {
			return err
		}
		freshResByName, freshConflicting := r.computeDSCGlobalStateFromItems(ctx, list.Items)

		current := &storagev1alpha1.DomainSpecificSnapshotController{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: d.Name}, current); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		gen = current.GetGeneration()
		freshRes := freshResByName[current.Name]
		acceptedStatus, acceptedReason, acceptedMsg := r.computeAccepted(current.Name, freshRes, freshConflicting)
		rbac := meta.FindStatusCondition(current.Status.Conditions, DSCConditionRBACReady)
		ready := computeDSCReady(acceptedStatus, rbac, gen)
		desired := buildDSCStatusConditions(gen, acceptedStatus, acceptedReason, acceptedMsg, ready, current.Status.Conditions)
		if dscConditionsSemanticallyEqual(current.Status.Conditions, desired) {
			return nil
		}
		current.Status.Conditions = desired
		return r.Client.Status().Update(ctx, current)
	})
}

func (r *DomainSpecificSnapshotControllerReconciler) computeAccepted(
	dscName string,
	res dscEntryResolution,
	conflicting map[string]struct{},
) (metav1.ConditionStatus, string, string) {
	if _, isConflict := conflicting[dscName]; isConflict {
		return metav1.ConditionFalse, DSCReasonKindConflict, "snapshot kind claimed by more than one DomainSpecificSnapshotController"
	}
	if res.duplicateSnapshotKind {
		return metav1.ConditionFalse, DSCReasonInvalidSpec, "duplicate snapshot kind in snapshotResourceMapping within the same DSC"
	}
	var errMsgs []string
	for _, m := range res.perMapping {
		if m.resolveErr != nil {
			errMsgs = append(errMsgs, m.resolveErr.Error())
		}
	}
	if len(errMsgs) > 0 {
		return metav1.ConditionFalse, DSCReasonInvalidSpec, strings.Join(errMsgs, "; ")
	}
	return metav1.ConditionTrue, "Resolved", "mapping resolved, content CRDs are cluster-scoped"
}

// computeDSCReady mirrors ADR: Ready=True iff Accepted=True, RBACReady=True, both observedGeneration == metadata.generation.
func computeDSCReady(accepted metav1.ConditionStatus, rbac *metav1.Condition, gen int64) metav1.ConditionStatus {
	if accepted != metav1.ConditionTrue {
		return metav1.ConditionFalse
	}
	if rbac == nil || rbac.Status != metav1.ConditionTrue || rbac.ObservedGeneration != gen {
		return metav1.ConditionFalse
	}
	return metav1.ConditionTrue
}

// buildDSCStatusConditions sets Accepted and Ready; copies RBACReady from existing if present.
func buildDSCStatusConditions(
	gen int64,
	acceptedStatus metav1.ConditionStatus,
	acceptedReason string,
	acceptedMessage string,
	readyStatus metav1.ConditionStatus,
	existing []metav1.Condition,
) []metav1.Condition {
	var conds []metav1.Condition

	meta.SetStatusCondition(&conds, metav1.Condition{
		Type:               DSCConditionAccepted,
		Status:             acceptedStatus,
		Reason:             acceptedReason,
		Message:            acceptedMessage,
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
	})

	if rbac := meta.FindStatusCondition(existing, DSCConditionRBACReady); rbac != nil {
		cp := *rbac
		meta.SetStatusCondition(&conds, cp)
	}

	readyReason := DSCReadyReasonNotReady
	var readyMsg string
	switch {
	case readyStatus == metav1.ConditionTrue:
		readyReason = "Active"
		readyMsg = "Accepted and RBACReady are True for current generation"
	case acceptedStatus != metav1.ConditionTrue:
		readyMsg = "Ready=False because condition Accepted is not True; see Accepted for details"
	default:
		readyMsg = "Waiting for RBACReady=True with observedGeneration matching metadata.generation (Deckhouse hook)"
	}

	meta.SetStatusCondition(&conds, metav1.Condition{
		Type:               DSCConditionReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMsg,
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
	})

	return conds
}

func dscConditionsSemanticallyEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[string]metav1.Condition, len(a))
	for _, c := range a {
		index[c.Type] = c
	}
	for _, want := range b {
		got, ok := index[want.Type]
		if !ok {
			return false
		}
		if !dscConditionFieldsEqual(got, want) {
			return false
		}
	}
	return true
}

func dscConditionFieldsEqual(x, y metav1.Condition) bool {
	return x.Status == y.Status && x.Reason == y.Reason && x.Message == y.Message && x.ObservedGeneration == y.ObservedGeneration
}

func (r *DomainSpecificSnapshotControllerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Logger == nil {
		return fmt.Errorf("Logger is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.DomainSpecificSnapshotController{}).
		Complete(r)
}
