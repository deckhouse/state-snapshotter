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

package csd

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

// CSD condition types (ADR: snapshot-rework/2026-01-23-unified-snapshots-registry.md).
const (
	CSDConditionAccepted            = "Accepted"
	CSDConditionSourceAccessGranted = "SourceAccessGranted"
	CSDConditionReady               = "Ready"
)

const (
	CSDReasonKindConflict = "KindConflict"
	CSDReasonInvalidSpec  = "InvalidSpec"
	// CSDReasonSnapshotContractUnsatisfied is set when a registered snapshot kind's CRD does not
	// expose the framework status protocol fields the generic orchestration reads by fixed name.
	CSDReasonSnapshotContractUnsatisfied = "SnapshotContractUnsatisfied"
	// CSDReadyReasonNotReady is the Ready condition reason when the aggregate is false (not a spec-level standardized reason).
	CSDReadyReasonNotReady = "NotReady"
)

// requiredSnapshotStatusFields are the framework status protocol fields every snapshot kind
// registered in a CSD must expose. Generic orchestration reads these by fixed canonical name;
// the domain spec stays opaque. status.childrenSnapshotRefs is part of the protocol for non-leaf
// snapshot kinds but is intentionally not enforced here: leaf snapshot kinds legitimately omit it
// and CSD cannot know leaf-ness at registration time.
var requiredSnapshotStatusFields = []string{
	"boundSnapshotContentName",
	"conditions",
}

// CustomSnapshotDefinitionReconciler resolves snapshotResourceMapping, detects cross-CSD
// snapshot kind conflicts, writes Accepted and aggregated Ready. SourceAccessGranted is owned by Deckhouse hook.
// Runtime watch activation is triggered after successful status reconciliation.
//
// Phase-1 trade-off: Reconcile ignores the triggering request and always List()s all CSDs, then fully
// recomputes resolution and conflicts for every object. Any update to one CSD re-runs the whole cycle.
// This is intentional for correctness and simplicity; optimize later if needed.
type CustomSnapshotDefinitionReconciler struct {
	Client     client.Client
	Scheme     *runtime.Scheme
	RESTMapper meta.RESTMapper
	Logger     logger.LoggerInterface
	Config     *config.Options

	// UnifiedRuntimeSync runs after a successful full CSD reconcile. Production wiring always provides it;
	// nil remains valid for focused unit tests.
	UnifiedRuntimeSync func(context.Context) error

	// GraphRegistryRefresh rebuilds the generic Snapshot graph GVK registry. Production wiring
	// always provides it; nil remains valid for focused unit tests.
	GraphRegistryRefresh func(context.Context) error
}

func NewCustomSnapshotDefinitionReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	log logger.LoggerInterface,
	cfg *config.Options,
) (*CustomSnapshotDefinitionReconciler, error) {
	if c == nil {
		return nil, fmt.Errorf("client must not be nil")
	}
	if scheme == nil {
		return nil, fmt.Errorf("scheme must not be nil")
	}
	if log == nil {
		return nil, fmt.Errorf("logger must not be nil")
	}
	return &CustomSnapshotDefinitionReconciler{
		Client: c,
		Scheme: scheme,
		Logger: log,
		Config: cfg,
	}, nil
}

func (r *CustomSnapshotDefinitionReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	var list storagev1alpha1.CustomSnapshotDefinitionList
	if err := r.Client.List(ctx, &list); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileAll(ctx, list.Items); err != nil {
		r.Logger.Error(err, "CSD reconcileAll failed")
		return ctrl.Result{}, err
	}
	if r.GraphRegistryRefresh != nil {
		if err := r.GraphRegistryRefresh(ctx); err != nil {
			r.Logger.Warning("snapshot graph registry refresh after CSD reconcile failed", "error", err)
		}
	}
	if r.UnifiedRuntimeSync != nil {
		if err := r.UnifiedRuntimeSync(ctx); err != nil {
			r.Logger.Warning("unified GVK runtime sync after CSD reconcile failed", "error", err)
		}
	}
	return ctrl.Result{}, nil
}

// csdEntryResolution is the resolution of a CSD's single snapshot mapping (flat schema).
type csdEntryResolution struct {
	snapshotGK  schema.GroupKind
	resolveErr  error
	contractErr error
}

func (r *CustomSnapshotDefinitionReconciler) reconcileAll(ctx context.Context, items []storagev1alpha1.CustomSnapshotDefinition) error {
	resByName, conflicting := r.computeCSDGlobalStateFromItems(ctx, items)
	for i := range items {
		d := &items[i]
		if err := r.writeStatusIfNeeded(ctx, d, resByName, conflicting); err != nil {
			return fmt.Errorf("update status for CSD %q: %w", d.Name, err)
		}
	}
	return nil
}

// computeCSDGlobalStateFromItems resolves every CSD spec and derives KindConflict participants.
func (r *CustomSnapshotDefinitionReconciler) computeCSDGlobalStateFromItems(
	ctx context.Context,
	items []storagev1alpha1.CustomSnapshotDefinition,
) (resByName map[string]csdEntryResolution, conflicting map[string]struct{}) {
	resByName = make(map[string]csdEntryResolution, len(items))
	for i := range items {
		d := &items[i]
		resByName[d.Name] = r.resolveCSDSpec(ctx, d)
	}

	snapshotOwners := make(map[string]map[string]struct{})
	for i := range items {
		d := &items[i]
		res := resByName[d.Name]
		if res.resolveErr != nil || res.contractErr != nil {
			continue
		}
		gkKey := res.snapshotGK.String()
		if snapshotOwners[gkKey] == nil {
			snapshotOwners[gkKey] = make(map[string]struct{})
		}
		snapshotOwners[gkKey][d.Name] = struct{}{}
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

func (r *CustomSnapshotDefinitionReconciler) resolveCSDSpec(ctx context.Context, d *storagev1alpha1.CustomSnapshotDefinition) csdEntryResolution {
	var out csdEntryResolution
	_, snapGVK, err := r.resolveMappingGVKs(ctx, d)
	if err != nil {
		out.resolveErr = err
		return out
	}
	out.snapshotGK = snapGVK.GroupKind()
	out.contractErr = r.validateSnapshotStatusContract(ctx, snapGVK)
	return out
}

func (r *CustomSnapshotDefinitionReconciler) resolveMappingGVKs(ctx context.Context, d *storagev1alpha1.CustomSnapshotDefinition) (schema.GroupVersionKind, schema.GroupVersionKind, error) {
	_ = ctx
	resourceGVK, err := gvkFromRef(d.Spec.Source)
	if err != nil {
		return schema.GroupVersionKind{}, schema.GroupVersionKind{}, fmt.Errorf("source GVK: %w", err)
	}
	snapGVK, err := gvkFromRef(storagev1alpha1.SnapshotGVKRef{APIVersion: d.Spec.APIVersion, Kind: d.Spec.Kind})
	if err != nil {
		return schema.GroupVersionKind{}, schema.GroupVersionKind{}, fmt.Errorf("snapshot GVK: %w", err)
	}
	if r.RESTMapper != nil {
		if _, err := r.RESTMapper.RESTMapping(resourceGVK.GroupKind(), resourceGVK.Version); err != nil {
			return schema.GroupVersionKind{}, schema.GroupVersionKind{}, fmt.Errorf("source RESTMapping %s: %w", resourceGVK.String(), err)
		}
		if _, err := r.RESTMapper.RESTMapping(snapGVK.GroupKind(), snapGVK.Version); err != nil {
			return schema.GroupVersionKind{}, schema.GroupVersionKind{}, fmt.Errorf("snapshot RESTMapping %s: %w", snapGVK.String(), err)
		}
	}
	return resourceGVK, snapGVK, nil
}

// validateSnapshotStatusContract performs a shallow check that the snapshot kind's CRD exposes the
// canonical framework status protocol fields the generic orchestration reads by fixed name. It only
// blocks (returns a non-nil error) when it can positively determine, from a structural schema, that
// the fields are absent. It is intentionally lenient (returns nil) when introspection is not possible:
// no RESTMapper (focused unit tests), kind not CRD-backed (built-in/aggregated; out of scope this
// slice), or status is schemaless / preserve-unknown.
func (r *CustomSnapshotDefinitionReconciler) validateSnapshotStatusContract(ctx context.Context, snapGVK schema.GroupVersionKind) error {
	if r.Client == nil || r.RESTMapper == nil {
		return nil
	}
	rm, err := r.RESTMapper.RESTMapping(snapGVK.GroupKind(), snapGVK.Version)
	if err != nil {
		// Resolution failure is already reported as InvalidSpec by resolveMappingGVKs.
		return nil
	}
	crdName := rm.Resource.Resource + "." + rm.Resource.Group
	crd := &extv1.CustomResourceDefinition{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: crdName}, crd); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return evaluateSnapshotStatusContract(crd, snapGVK)
}

// evaluateSnapshotStatusContract is the pure contract decision: given a fetched CRD and the snapshot
// GVK, it returns a non-nil error only when it can positively determine the framework status contract
// is violated. It stays lenient when the schema is not structurally introspectable.
func evaluateSnapshotStatusContract(crd *extv1.CustomResourceDefinition, snapGVK schema.GroupVersionKind) error {
	statusSchema, inspectable, statusDeclared := crdStatusSchemaForVersion(crd, snapGVK.Version)
	if !inspectable {
		// Schemaless / preserve-unknown / version not described structurally: cannot reason about
		// field presence, stay lenient.
		return nil
	}
	if !statusDeclared {
		// Structural schema that declares no status block at all positively violates the contract.
		return contractUnsatisfiedError(snapGVK, allRequiredStatusPaths())
	}
	if missing := missingSnapshotStatusContractFields(statusSchema); len(missing) > 0 {
		return contractUnsatisfiedError(snapGVK, missing)
	}
	return nil
}

func allRequiredStatusPaths() []string {
	paths := make([]string, 0, len(requiredSnapshotStatusFields))
	for _, field := range requiredSnapshotStatusFields {
		paths = append(paths, "status."+field)
	}
	return paths
}

func contractUnsatisfiedError(snapGVK schema.GroupVersionKind, missing []string) error {
	return fmt.Errorf("snapshot kind %s does not expose framework status contract fields: %s", snapGVK.String(), strings.Join(missing, ", "))
}

// missingSnapshotStatusContractFields returns the canonical status fields absent from an already
// declared structural status subschema. It is lenient (returns nil) when the status subschema itself
// is opaque: nil/empty properties or preserve-unknown. The "status block entirely absent" case is
// handled earlier by validateSnapshotStatusContract via crdStatusSchemaForVersion's statusDeclared
// flag, so it is not this function's responsibility. This keeps the check shallow (presence of
// canonical paths, not full semantic schema).
func missingSnapshotStatusContractFields(statusSchema *extv1.JSONSchemaProps) []string {
	if statusSchema == nil || len(statusSchema.Properties) == 0 {
		return nil
	}
	if statusSchema.XPreserveUnknownFields != nil && *statusSchema.XPreserveUnknownFields {
		return nil
	}
	var missing []string
	for _, field := range requiredSnapshotStatusFields {
		if _, ok := statusSchema.Properties[field]; !ok {
			missing = append(missing, "status."+field)
		}
	}
	return missing
}

// crdStatusSchemaForVersion inspects the openAPIV3Schema for the given served version.
//   - inspectable reports whether the version is described by a structural object schema we can reason
//     about (so the absence of a field is meaningful). It is false for missing versions, absent schema,
//     or a root that preserves unknown fields / declares no properties (schemaless).
//   - statusDeclared reports whether a "status" property is present in that structural schema.
//   - statusSchema is the "status" subschema when statusDeclared is true.
func crdStatusSchemaForVersion(crd *extv1.CustomResourceDefinition, version string) (statusSchema *extv1.JSONSchemaProps, inspectable bool, statusDeclared bool) {
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Name != version {
			continue
		}
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			return nil, false, false
		}
		root := v.Schema.OpenAPIV3Schema
		if (root.XPreserveUnknownFields != nil && *root.XPreserveUnknownFields) || len(root.Properties) == 0 {
			return nil, false, false
		}
		status, ok := root.Properties["status"]
		if !ok {
			return nil, true, false
		}
		return &status, true, true
	}
	return nil, false, false
}

func gvkFromRef(ref storagev1alpha1.SnapshotGVKRef) (schema.GroupVersionKind, error) {
	if ref.APIVersion == "" || ref.Kind == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("source/snapshot apiVersion and kind are required")
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	return gv.WithKind(ref.Kind), nil
}

func (r *CustomSnapshotDefinitionReconciler) writeStatusIfNeeded(
	ctx context.Context,
	d *storagev1alpha1.CustomSnapshotDefinition,
	resByName map[string]csdEntryResolution,
	conflicting map[string]struct{},
) error {
	res := resByName[d.Name]
	gen := d.GetGeneration()

	acceptedStatus, acceptedReason, acceptedMsg := r.computeAccepted(d.Name, res, conflicting)
	rbac := meta.FindStatusCondition(d.Status.Conditions, CSDConditionSourceAccessGranted)
	ready := computeCSDReady(acceptedStatus, rbac, gen)

	desired := buildCSDStatusConditions(gen, acceptedStatus, acceptedReason, acceptedMsg, ready, d.Status.Conditions)

	if csdConditionsSemanticallyEqual(d.Status.Conditions, desired) {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var list storagev1alpha1.CustomSnapshotDefinitionList
		if err := r.Client.List(ctx, &list); err != nil {
			return err
		}
		freshResByName, freshConflicting := r.computeCSDGlobalStateFromItems(ctx, list.Items)

		current := &storagev1alpha1.CustomSnapshotDefinition{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: d.Name}, current); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		gen = current.GetGeneration()
		freshRes := freshResByName[current.Name]
		acceptedStatus, acceptedReason, acceptedMsg := r.computeAccepted(current.Name, freshRes, freshConflicting)
		rbac := meta.FindStatusCondition(current.Status.Conditions, CSDConditionSourceAccessGranted)
		ready := computeCSDReady(acceptedStatus, rbac, gen)
		desired := buildCSDStatusConditions(gen, acceptedStatus, acceptedReason, acceptedMsg, ready, current.Status.Conditions)
		if csdConditionsSemanticallyEqual(current.Status.Conditions, desired) {
			return nil
		}
		current.Status.Conditions = desired
		return r.Client.Status().Update(ctx, current)
	})
}

func (r *CustomSnapshotDefinitionReconciler) computeAccepted(
	csdName string,
	res csdEntryResolution,
	conflicting map[string]struct{},
) (metav1.ConditionStatus, string, string) {
	if _, isConflict := conflicting[csdName]; isConflict {
		return metav1.ConditionFalse, CSDReasonKindConflict, "snapshot kind claimed by more than one CustomSnapshotDefinition"
	}
	if res.resolveErr != nil {
		return metav1.ConditionFalse, CSDReasonInvalidSpec, res.resolveErr.Error()
	}
	if res.contractErr != nil {
		return metav1.ConditionFalse, CSDReasonSnapshotContractUnsatisfied, res.contractErr.Error()
	}
	return metav1.ConditionTrue, "Resolved", "mapping resolved, content CRDs are cluster-scoped"
}

// computeCSDReady mirrors ADR: Ready=True iff Accepted=True, SourceAccessGranted=True, both observedGeneration == metadata.generation.
func computeCSDReady(accepted metav1.ConditionStatus, rbac *metav1.Condition, gen int64) metav1.ConditionStatus {
	if accepted != metav1.ConditionTrue {
		return metav1.ConditionFalse
	}
	if rbac == nil || rbac.Status != metav1.ConditionTrue || rbac.ObservedGeneration != gen {
		return metav1.ConditionFalse
	}
	return metav1.ConditionTrue
}

// buildCSDStatusConditions sets Accepted and Ready; copies SourceAccessGranted from existing if present.
func buildCSDStatusConditions(
	gen int64,
	acceptedStatus metav1.ConditionStatus,
	acceptedReason string,
	acceptedMessage string,
	readyStatus metav1.ConditionStatus,
	existing []metav1.Condition,
) []metav1.Condition {
	var conds []metav1.Condition

	meta.SetStatusCondition(&conds, metav1.Condition{
		Type:               CSDConditionAccepted,
		Status:             acceptedStatus,
		Reason:             acceptedReason,
		Message:            acceptedMessage,
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
	})

	if rbac := meta.FindStatusCondition(existing, CSDConditionSourceAccessGranted); rbac != nil {
		cp := *rbac
		meta.SetStatusCondition(&conds, cp)
	}

	readyReason := CSDReadyReasonNotReady
	var readyMsg string
	switch {
	case readyStatus == metav1.ConditionTrue:
		readyReason = "Active"
		readyMsg = "Accepted and SourceAccessGranted are True for current generation"
	case acceptedStatus != metav1.ConditionTrue:
		readyMsg = "Ready=False because condition Accepted is not True; see Accepted for details"
	default:
		readyMsg = "Waiting for SourceAccessGranted=True with observedGeneration matching metadata.generation (Deckhouse hook)"
	}

	meta.SetStatusCondition(&conds, metav1.Condition{
		Type:               CSDConditionReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMsg,
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Now(),
	})

	return conds
}

func csdConditionsSemanticallyEqual(a, b []metav1.Condition) bool {
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
		if !csdConditionFieldsEqual(got, want) {
			return false
		}
	}
	return true
}

func csdConditionFieldsEqual(x, y metav1.Condition) bool {
	return x.Status == y.Status && x.Reason == y.Reason && x.Message == y.Message && x.ObservedGeneration == y.ObservedGeneration
}

func (r *CustomSnapshotDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Logger == nil {
		return fmt.Errorf("Logger is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.CustomSnapshotDefinition{}).
		Complete(r)
}
