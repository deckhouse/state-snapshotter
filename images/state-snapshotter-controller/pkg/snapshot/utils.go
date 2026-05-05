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

package snapshot

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// FinalizerParentProtect prevents deletion of SnapshotContent
	// while a parent Snapshot exists. This ensures that SnapshotContent
	// cannot be manually deleted (via kubectl delete) while the parent
	// Snapshot is still alive.
	FinalizerParentProtect = "snapshot.deckhouse.io/parent-protect"
	// FinalizerArtifactProtect prevents deletion of artifacts (MCP/VSC)
	// while they are linked from SnapshotContent.
	FinalizerArtifactProtect = "snapshot.deckhouse.io/artifact-protect"
	// FinalizerNamespaceSnapshot blocks NamespaceSnapshot deletion until cleanup completes.
	FinalizerNamespaceSnapshot = "namespacesnapshot.finalizers.deckhouse.io"
	// AnnotationParentDeleted marks SnapshotContent as detached from parent Snapshot.
	AnnotationParentDeleted = "snapshot.deckhouse.io/parent-deleted"
)

// ExtractSnapshotLike converts an unstructured object to SnapshotLike interface
// This function uses dynamic client to read the object, then converts it to typed interface
func ExtractSnapshotLike(obj *unstructured.Unstructured) (SnapshotLike, error) {
	// For now, we'll use a wrapper that implements SnapshotLike
	// In the future, domain controllers can provide their own implementations
	return &unstructuredSnapshotWrapper{obj: obj}, nil
}

// ExtractSnapshotContentLike converts an unstructured object to SnapshotContentLike interface
func ExtractSnapshotContentLike(obj *unstructured.Unstructured) (SnapshotContentLike, error) {
	return &unstructuredSnapshotContentWrapper{obj: obj}, nil
}

// unstructuredSnapshotWrapper implements SnapshotLike for unstructured objects
// This allows the common controller to work with any snapshot type
type unstructuredSnapshotWrapper struct {
	obj *unstructured.Unstructured
}

func (w *unstructuredSnapshotWrapper) GetObjectKind() schema.ObjectKind {
	return w.obj.GetObjectKind()
}

func (w *unstructuredSnapshotWrapper) DeepCopyObject() runtime.Object {
	return &unstructuredSnapshotWrapper{obj: w.obj.DeepCopy()}
}

func (w *unstructuredSnapshotWrapper) GetObjectMeta() metav1.Object {
	return w.obj
}

// Delegate metav1.Object methods to underlying unstructured object
func (w *unstructuredSnapshotWrapper) GetAnnotations() map[string]string {
	return w.obj.GetAnnotations()
}

func (w *unstructuredSnapshotWrapper) SetAnnotations(annotations map[string]string) {
	w.obj.SetAnnotations(annotations)
}

func (w *unstructuredSnapshotWrapper) GetLabels() map[string]string {
	return w.obj.GetLabels()
}

func (w *unstructuredSnapshotWrapper) SetLabels(labels map[string]string) {
	w.obj.SetLabels(labels)
}

func (w *unstructuredSnapshotWrapper) GetFinalizers() []string {
	return w.obj.GetFinalizers()
}

func (w *unstructuredSnapshotWrapper) SetFinalizers(finalizers []string) {
	w.obj.SetFinalizers(finalizers)
}

func (w *unstructuredSnapshotWrapper) GetOwnerReferences() []metav1.OwnerReference {
	return w.obj.GetOwnerReferences()
}

func (w *unstructuredSnapshotWrapper) SetOwnerReferences(references []metav1.OwnerReference) {
	w.obj.SetOwnerReferences(references)
}

func (w *unstructuredSnapshotWrapper) GetManagedFields() []metav1.ManagedFieldsEntry {
	return w.obj.GetManagedFields()
}

func (w *unstructuredSnapshotWrapper) SetManagedFields(managedFields []metav1.ManagedFieldsEntry) {
	w.obj.SetManagedFields(managedFields)
}

func (w *unstructuredSnapshotWrapper) GetCreationTimestamp() metav1.Time {
	return w.obj.GetCreationTimestamp()
}

func (w *unstructuredSnapshotWrapper) SetCreationTimestamp(timestamp metav1.Time) {
	w.obj.SetCreationTimestamp(timestamp)
}

func (w *unstructuredSnapshotWrapper) GetDeletionTimestamp() *metav1.Time {
	return w.obj.GetDeletionTimestamp()
}

func (w *unstructuredSnapshotWrapper) SetDeletionTimestamp(timestamp *metav1.Time) {
	w.obj.SetDeletionTimestamp(timestamp)
}

func (w *unstructuredSnapshotWrapper) GetDeletionGracePeriodSeconds() *int64 {
	return w.obj.GetDeletionGracePeriodSeconds()
}

func (w *unstructuredSnapshotWrapper) SetDeletionGracePeriodSeconds(gracePeriodSeconds *int64) {
	w.obj.SetDeletionGracePeriodSeconds(gracePeriodSeconds)
}

func (w *unstructuredSnapshotWrapper) GetGeneration() int64 {
	return w.obj.GetGeneration()
}

func (w *unstructuredSnapshotWrapper) SetGeneration(generation int64) {
	w.obj.SetGeneration(generation)
}

func (w *unstructuredSnapshotWrapper) GetName() string {
	return w.obj.GetName()
}

func (w *unstructuredSnapshotWrapper) SetName(name string) {
	w.obj.SetName(name)
}

func (w *unstructuredSnapshotWrapper) GetGenerateName() string {
	return w.obj.GetGenerateName()
}

func (w *unstructuredSnapshotWrapper) SetGenerateName(name string) {
	w.obj.SetGenerateName(name)
}

func (w *unstructuredSnapshotWrapper) GetNamespace() string {
	return w.obj.GetNamespace()
}

func (w *unstructuredSnapshotWrapper) SetNamespace(namespace string) {
	w.obj.SetNamespace(namespace)
}

func (w *unstructuredSnapshotWrapper) GetUID() types.UID {
	return w.obj.GetUID()
}

func (w *unstructuredSnapshotWrapper) SetUID(uid types.UID) {
	w.obj.SetUID(uid)
}

func (w *unstructuredSnapshotWrapper) GetResourceVersion() string {
	return w.obj.GetResourceVersion()
}

func (w *unstructuredSnapshotWrapper) SetResourceVersion(version string) {
	w.obj.SetResourceVersion(version)
}

func (w *unstructuredSnapshotWrapper) GetSelfLink() string {
	return w.obj.GetSelfLink()
}

func (w *unstructuredSnapshotWrapper) SetSelfLink(selfLink string) {
	w.obj.SetSelfLink(selfLink)
}

func (w *unstructuredSnapshotWrapper) GetSpecSnapshotRef() *ObjectRef {
	// Extract from spec.xxxxName or spec.snapshotRef
	// NOTE: Common controller should NOT guess domain-specific field names
	// If this returns nil, it means either:
	// 1. This is a root snapshot (no source object)
	// 2. Domain controller should populate canonical spec.snapshotRef
	spec, ok := w.obj.Object["spec"].(map[string]interface{})
	if !ok {
		return nil
	}

	// First, try canonical spec.snapshotRef (preferred)
	if snapshotRefRaw, ok := spec["snapshotRef"].(map[string]interface{}); ok {
		ref := &ObjectRef{}
		if kind, ok := snapshotRefRaw["kind"].(string); ok {
			ref.Kind = kind
		}
		if name, ok := snapshotRefRaw["name"].(string); ok {
			ref.Name = name
		}
		if ns, ok := snapshotRefRaw["namespace"].(string); ok {
			ref.Namespace = ns
		} else {
			ref.Namespace = w.obj.GetNamespace()
		}
		if ref.Kind != "" && ref.Name != "" {
			return ref
		}
	}

	// If no canonical ref, return nil
	// Domain controllers should populate spec.snapshotRef for non-root snapshots
	return nil
}

func (w *unstructuredSnapshotWrapper) GetStatusContentName() string {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	// Unified bind field for all snapshot root kinds (content GVK comes from pairing / controller, not from the field name).
	if name, ok := status["boundSnapshotContentName"].(string); ok && name != "" {
		return name
	}
	return ""
}

func (w *unstructuredSnapshotWrapper) GetStatusManifestCaptureRequestName() string {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	// Try both namespaced and cluster-scoped field names
	if name, ok := status["manifestCaptureRequestName"].(string); ok {
		return name
	}
	if name, ok := status["clusterManifestCaptureRequestName"].(string); ok {
		return name
	}
	return ""
}

func (w *unstructuredSnapshotWrapper) GetStatusVolumeCaptureRequestName() string {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	if name, ok := status["volumeCaptureRequestName"].(string); ok {
		return name
	}
	return ""
}

func (w *unstructuredSnapshotWrapper) GetStatusChildrenSnapshotRefs() []ObjectRef {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return nil
	}
	childrenRaw, ok := status["childrenSnapshotRefs"].([]interface{})
	if !ok {
		return nil
	}

	var refs []ObjectRef
	for _, childRaw := range childrenRaw {
		child, ok := childRaw.(map[string]interface{})
		if !ok {
			continue
		}
		ref := ObjectRef{}
		if kind, ok := child["kind"].(string); ok {
			ref.Kind = kind
		}
		if name, ok := child["name"].(string); ok {
			ref.Name = name
		}
		if ns, ok := child["namespace"].(string); ok {
			ref.Namespace = ns
		} else {
			// If namespace not specified, use parent's namespace for namespaced resources
			ref.Namespace = w.obj.GetNamespace()
		}
		refs = append(refs, ref)
	}
	return refs
}

func (w *unstructuredSnapshotWrapper) GetStatusConditions() []metav1.Condition {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return nil
	}
	conditionsRaw, ok := status["conditions"].([]interface{})
	if !ok {
		return nil
	}

	var conditions []metav1.Condition
	for _, condRaw := range conditionsRaw {
		condMap, ok := condRaw.(map[string]interface{})
		if !ok {
			continue
		}
		cond := metav1.Condition{}
		if typ, ok := condMap["type"].(string); ok {
			cond.Type = typ
		}
		if status, ok := condMap["status"].(string); ok {
			cond.Status = metav1.ConditionStatus(status)
		}
		if reason, ok := condMap["reason"].(string); ok {
			cond.Reason = reason
		}
		if message, ok := condMap["message"].(string); ok {
			cond.Message = message
		}
		if observed, ok := condMap["observedGeneration"].(int64); ok {
			cond.ObservedGeneration = observed
		}
		if lastTransition, ok := condMap["lastTransitionTime"].(string); ok {
			if t, err := time.Parse(time.RFC3339, lastTransition); err == nil {
				cond.LastTransitionTime = metav1.NewTime(t)
			}
		}
		conditions = append(conditions, cond)
	}
	return conditions
}

func (w *unstructuredSnapshotWrapper) SetStatusConditions(conditions []metav1.Condition) {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		status = make(map[string]interface{})
		w.obj.Object["status"] = status
	}

	conditionsRaw := make([]interface{}, len(conditions))
	for i, cond := range conditions {
		conditionsRaw[i] = map[string]interface{}{
			"type":               cond.Type,
			"status":             string(cond.Status),
			"reason":             cond.Reason,
			"message":            cond.Message,
			"observedGeneration": cond.ObservedGeneration,
			"lastTransitionTime": cond.LastTransitionTime.Format(time.RFC3339),
		}
	}
	status["conditions"] = conditionsRaw
}

func (w *unstructuredSnapshotWrapper) GetStatusDataConsistency() string {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	if consistency, ok := status["dataConsistency"].(string); ok {
		return consistency
	}
	return ""
}

func (w *unstructuredSnapshotWrapper) GetStatusDataSnapshotMethod() string {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	if method, ok := status["dataSnapshotMethod"].(string); ok {
		return method
	}
	return ""
}

func (w *unstructuredSnapshotWrapper) IsNamespaced() bool {
	return w.obj.GetNamespace() != ""
}

// unstructuredSnapshotContentWrapper implements SnapshotContentLike for unstructured objects
type unstructuredSnapshotContentWrapper struct {
	obj *unstructured.Unstructured
}

func (w *unstructuredSnapshotContentWrapper) GetObjectKind() schema.ObjectKind {
	return w.obj.GetObjectKind()
}

func (w *unstructuredSnapshotContentWrapper) DeepCopyObject() runtime.Object {
	return &unstructuredSnapshotContentWrapper{obj: w.obj.DeepCopy()}
}

func (w *unstructuredSnapshotContentWrapper) GetObjectMeta() metav1.Object {
	return w.obj
}

// Delegate metav1.Object methods to underlying unstructured object
func (w *unstructuredSnapshotContentWrapper) GetAnnotations() map[string]string {
	return w.obj.GetAnnotations()
}

func (w *unstructuredSnapshotContentWrapper) SetAnnotations(annotations map[string]string) {
	w.obj.SetAnnotations(annotations)
}

func (w *unstructuredSnapshotContentWrapper) GetLabels() map[string]string {
	return w.obj.GetLabels()
}

func (w *unstructuredSnapshotContentWrapper) SetLabels(labels map[string]string) {
	w.obj.SetLabels(labels)
}

func (w *unstructuredSnapshotContentWrapper) GetFinalizers() []string {
	return w.obj.GetFinalizers()
}

func (w *unstructuredSnapshotContentWrapper) SetFinalizers(finalizers []string) {
	w.obj.SetFinalizers(finalizers)
}

func (w *unstructuredSnapshotContentWrapper) GetOwnerReferences() []metav1.OwnerReference {
	return w.obj.GetOwnerReferences()
}

func (w *unstructuredSnapshotContentWrapper) SetOwnerReferences(references []metav1.OwnerReference) {
	w.obj.SetOwnerReferences(references)
}

func (w *unstructuredSnapshotContentWrapper) GetManagedFields() []metav1.ManagedFieldsEntry {
	return w.obj.GetManagedFields()
}

func (w *unstructuredSnapshotContentWrapper) SetManagedFields(managedFields []metav1.ManagedFieldsEntry) {
	w.obj.SetManagedFields(managedFields)
}

func (w *unstructuredSnapshotContentWrapper) GetCreationTimestamp() metav1.Time {
	return w.obj.GetCreationTimestamp()
}

func (w *unstructuredSnapshotContentWrapper) SetCreationTimestamp(timestamp metav1.Time) {
	w.obj.SetCreationTimestamp(timestamp)
}

func (w *unstructuredSnapshotContentWrapper) GetDeletionTimestamp() *metav1.Time {
	return w.obj.GetDeletionTimestamp()
}

func (w *unstructuredSnapshotContentWrapper) SetDeletionTimestamp(timestamp *metav1.Time) {
	w.obj.SetDeletionTimestamp(timestamp)
}

func (w *unstructuredSnapshotContentWrapper) GetDeletionGracePeriodSeconds() *int64 {
	return w.obj.GetDeletionGracePeriodSeconds()
}

func (w *unstructuredSnapshotContentWrapper) SetDeletionGracePeriodSeconds(gracePeriodSeconds *int64) {
	w.obj.SetDeletionGracePeriodSeconds(gracePeriodSeconds)
}

func (w *unstructuredSnapshotContentWrapper) GetGeneration() int64 {
	return w.obj.GetGeneration()
}

func (w *unstructuredSnapshotContentWrapper) SetGeneration(generation int64) {
	w.obj.SetGeneration(generation)
}

func (w *unstructuredSnapshotContentWrapper) GetName() string {
	return w.obj.GetName()
}

func (w *unstructuredSnapshotContentWrapper) SetName(name string) {
	w.obj.SetName(name)
}

func (w *unstructuredSnapshotContentWrapper) GetGenerateName() string {
	return w.obj.GetGenerateName()
}

func (w *unstructuredSnapshotContentWrapper) SetGenerateName(name string) {
	w.obj.SetGenerateName(name)
}

func (w *unstructuredSnapshotContentWrapper) GetNamespace() string {
	return w.obj.GetNamespace()
}

func (w *unstructuredSnapshotContentWrapper) SetNamespace(namespace string) {
	w.obj.SetNamespace(namespace)
}

func (w *unstructuredSnapshotContentWrapper) GetUID() types.UID {
	return w.obj.GetUID()
}

func (w *unstructuredSnapshotContentWrapper) SetUID(uid types.UID) {
	w.obj.SetUID(uid)
}

func (w *unstructuredSnapshotContentWrapper) GetResourceVersion() string {
	return w.obj.GetResourceVersion()
}

func (w *unstructuredSnapshotContentWrapper) SetResourceVersion(version string) {
	w.obj.SetResourceVersion(version)
}

func (w *unstructuredSnapshotContentWrapper) GetSelfLink() string {
	return w.obj.GetSelfLink()
}

func (w *unstructuredSnapshotContentWrapper) SetSelfLink(selfLink string) {
	w.obj.SetSelfLink(selfLink)
}

func (w *unstructuredSnapshotContentWrapper) GetSpecSnapshotRef() *ObjectRef {
	spec, ok := w.obj.Object["spec"].(map[string]interface{})
	if !ok {
		return nil
	}
	snapshotRefRaw, ok := spec["snapshotRef"].(map[string]interface{})
	if !ok {
		return nil
	}

	ref := &ObjectRef{}
	if kind, ok := snapshotRefRaw["kind"].(string); ok {
		ref.Kind = kind
	}
	if name, ok := snapshotRefRaw["name"].(string); ok {
		ref.Name = name
	}
	if ns, ok := snapshotRefRaw["namespace"].(string); ok {
		ref.Namespace = ns
	}
	return ref
}

func (w *unstructuredSnapshotContentWrapper) GetStatusManifestCheckpointName() string {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	if name, ok := status["manifestCheckpointName"].(string); ok {
		return name
	}
	return ""
}

func (w *unstructuredSnapshotContentWrapper) GetStatusDataRef() *ObjectRef {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return nil
	}
	dataRefRaw, ok := status["dataRef"].(map[string]interface{})
	if !ok {
		return nil
	}

	ref := &ObjectRef{}
	if apiVersion, ok := dataRefRaw["apiVersion"].(string); ok {
		ref.APIVersion = apiVersion
	}
	if kind, ok := dataRefRaw["kind"].(string); ok {
		ref.Kind = kind
	}
	if name, ok := dataRefRaw["name"].(string); ok {
		ref.Name = name
	}
	return ref
}

func (w *unstructuredSnapshotContentWrapper) GetStatusChildrenSnapshotContentRefs() []ObjectRef {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return nil
	}
	childrenRaw, ok := status["childrenSnapshotContentRefs"].([]interface{})
	if !ok {
		return nil
	}

	var refs []ObjectRef
	for _, childRaw := range childrenRaw {
		child, ok := childRaw.(map[string]interface{})
		if !ok {
			continue
		}
		ref := ObjectRef{}
		if kind, ok := child["kind"].(string); ok {
			ref.Kind = kind
		}
		if name, ok := child["name"].(string); ok {
			ref.Name = name
		}
		refs = append(refs, ref)
	}
	return refs
}

func (w *unstructuredSnapshotContentWrapper) GetStatusConditions() []metav1.Condition {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return nil
	}
	conditionsRaw, ok := status["conditions"].([]interface{})
	if !ok {
		return nil
	}

	var conditions []metav1.Condition
	for _, condRaw := range conditionsRaw {
		condMap, ok := condRaw.(map[string]interface{})
		if !ok {
			continue
		}
		cond := metav1.Condition{}
		if typ, ok := condMap["type"].(string); ok {
			cond.Type = typ
		}
		if status, ok := condMap["status"].(string); ok {
			cond.Status = metav1.ConditionStatus(status)
		}
		if reason, ok := condMap["reason"].(string); ok {
			cond.Reason = reason
		}
		if message, ok := condMap["message"].(string); ok {
			cond.Message = message
		}
		if observed, ok := condMap["observedGeneration"].(int64); ok {
			cond.ObservedGeneration = observed
		}
		if lastTransition, ok := condMap["lastTransitionTime"].(string); ok {
			if t, err := time.Parse(time.RFC3339, lastTransition); err == nil {
				cond.LastTransitionTime = metav1.NewTime(t)
			}
		}
		conditions = append(conditions, cond)
	}
	return conditions
}

func (w *unstructuredSnapshotContentWrapper) SetStatusConditions(conditions []metav1.Condition) {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		status = make(map[string]interface{})
		w.obj.Object["status"] = status
	}

	conditionsRaw := make([]interface{}, len(conditions))
	for i, cond := range conditions {
		conditionsRaw[i] = map[string]interface{}{
			"type":               cond.Type,
			"status":             string(cond.Status),
			"reason":             cond.Reason,
			"message":            cond.Message,
			"observedGeneration": cond.ObservedGeneration,
			"lastTransitionTime": cond.LastTransitionTime.Format(time.RFC3339),
		}
	}
	status["conditions"] = conditionsRaw
}

func (w *unstructuredSnapshotContentWrapper) GetStatusDataConsistency() string {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	if consistency, ok := status["dataConsistency"].(string); ok {
		return consistency
	}
	return ""
}

func (w *unstructuredSnapshotContentWrapper) GetStatusDataSnapshotMethod() string {
	status, ok := w.obj.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	if method, ok := status["dataSnapshotMethod"].(string); ok {
		return method
	}
	return ""
}

// GenerateSnapshotContentName generates a deterministic name for SnapshotContent
// based on Snapshot name and UID
func GenerateSnapshotContentName(snapshotName, snapshotUID string) string {
	// Use first 8 characters of UID for uniqueness
	uidSuffix := snapshotUID
	if len(uidSuffix) > 8 {
		uidSuffix = uidSuffix[:8]
	}
	return fmt.Sprintf("%s-content-%s", snapshotName, strings.ToLower(uidSuffix))
}

// GenerateObjectKeeperName generates a deterministic name for ObjectKeeper
// for root snapshots
func GenerateObjectKeeperName(snapshotKind, snapshotName string) string {
	return fmt.Sprintf("ret-%s-%s", strings.ToLower(snapshotKind), snapshotName)
}

// IsRootSnapshot checks if a snapshot is a root (has no parent ownerRef)
func IsRootSnapshot(obj metav1.Object) bool {
	ownerRefs := obj.GetOwnerReferences()
	for _, ref := range ownerRefs {
		// Check if owner is another snapshot type
		// Common patterns: ends with "Snapshot" or has specific annotation
		if strings.HasSuffix(ref.Kind, "Snapshot") {
			return false
		}
	}
	return true
}
