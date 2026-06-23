package domain_rbac

import (
	"context"
	"fmt"
	"sort"

	"github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// buildRules builds a deterministic (sorted by group, resources within group)
// set of PolicyRules covering source and snapshot GVRs from all eligible CSDs.
// Rules are classified as PERMANENT or TEMPORARY; see the package-level comment.
func buildRules(sourceGVRs, snapshotGVRs []schema.GroupVersionResource) []rbacv1.PolicyRule {
	if len(sourceGVRs) == 0 && len(snapshotGVRs) == 0 {
		return nil
	}

	type groupEntry struct {
		sources   []string
		snapshots []string
	}
	byGroup := make(map[string]*groupEntry)
	var groupOrder []string

	ensureGroup := func(g string) {
		if _, ok := byGroup[g]; !ok {
			byGroup[g] = &groupEntry{}
			groupOrder = append(groupOrder, g)
		}
	}
	for _, gvr := range sourceGVRs {
		ensureGroup(gvr.Group)
		byGroup[gvr.Group].sources = append(byGroup[gvr.Group].sources, gvr.Resource)
	}
	for _, gvr := range snapshotGVRs {
		ensureGroup(gvr.Group)
		byGroup[gvr.Group].snapshots = append(byGroup[gvr.Group].snapshots, gvr.Resource)
	}

	sort.Strings(groupOrder)

	var rules []rbacv1.PolicyRule
	for _, g := range groupOrder {
		entry := byGroup[g]
		// Two CSDs can map to the same GVR; dedup so the rule's Resources slice is deterministic and
		// minimal (consistent with buildCoreReadRules / the subresource builders).
		entry.sources = sortedUnique(entry.sources)
		entry.snapshots = sortedUnique(entry.snapshots)

		if len(entry.sources) > 0 {
			// The domain snapshot controllers read each source object (referenced by the child
			// snapshot's spec.sourceRef, e.g. DemoVirtualDisk/DemoVirtualMachine) to capture it.
			// Read-only here; creation/ownership of the snapshot CRs is the snapshot-GVR rule below.
			// The CORE SA also needs source read for parent-graph planning — granted separately in
			// buildCoreSourceReadRules.
			rules = append(rules, rbacv1.PolicyRule{
				APIGroups: []string{g},
				Resources: entry.sources,
				Verbs:     []string{"get", "list", "watch"},
			})
		}

		if len(entry.snapshots) > 0 {
			statusResources := make([]string, len(entry.snapshots))
			finalizerResources := make([]string, len(entry.snapshots))
			for i, r := range entry.snapshots {
				statusResources[i] = r + "/status"
				finalizerResources[i] = r + "/finalizers"
			}

			rules = append(rules, rbacv1.PolicyRule{
				APIGroups: []string{g},
				Resources: entry.snapshots,
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			})

			rules = append(rules, rbacv1.PolicyRule{
				APIGroups: []string{g},
				Resources: statusResources,
				Verbs:     []string{"get", "update", "patch"},
			})

			rules = append(rules, rbacv1.PolicyRule{
				APIGroups: []string{g},
				Resources: finalizerResources,
				Verbs:     []string{"update", "patch"},
			})
		}
	}
	return rules
}

// buildCoreReadRules builds rules for the CORE SA on the dynamic demo snapshot GVRs:
//   - get/list/watch + create + patch on the snapshot resource. The core SnapshotReconciler is the
//     parent-graph planner: it CREATES one parent-owned child snapshot per source object
//     (parent_graph.go:ensureParentOwnedChildSnapshot → r.Client.Create) and PATCHes it to maintain the
//     ownerRef back to the root Snapshot. Without create the planner fails with
//     ChildrenSnapshotReady=False/GraphPlanningFailed ("cannot create demovirtualmachinesnapshots …").
//     The ownerRef does not set blockOwnerDeletion, so no /finalizers permission is required on the owner.
//   - status-write (get/update/patch on /status): binding BoundSnapshotContentName + volume-metadata
//     projection, co-owned via D4a.
//
// It still grants NO delete on the snapshot GVRs (child cleanup is ownerRef GC, not an explicit core
// delete) and NO /finalizers — those remain the domain SA's. These resource names are domain-specific
// (from CSD), so they cannot live in the static, domain-agnostic core RBAC and must be generated here.
func buildCoreReadRules(snapshotGVRs []schema.GroupVersionResource) []rbacv1.PolicyRule {
	if len(snapshotGVRs) == 0 {
		return nil
	}
	byGroup := make(map[string][]string)
	var groupOrder []string
	for _, gvr := range snapshotGVRs {
		if _, ok := byGroup[gvr.Group]; !ok {
			groupOrder = append(groupOrder, gvr.Group)
		}
		byGroup[gvr.Group] = append(byGroup[gvr.Group], gvr.Resource)
	}
	sort.Strings(groupOrder)

	var rules []rbacv1.PolicyRule
	for _, g := range groupOrder {
		resources := sortedUnique(byGroup[g])
		statusResources := make([]string, len(resources))
		for i, r := range resources {
			statusResources[i] = r + "/status"
		}
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{g},
			Resources: resources,
			Verbs:     []string{"get", "list", "watch", "create", "patch"},
		})
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{g},
			Resources: statusResources,
			Verbs:     []string{"get", "update", "patch"},
		})
	}
	return rules
}

// buildCoreSourceReadRules grants the CORE SA list on the dynamic demo SOURCE GVRs. The core
// SnapshotReconciler enumerates the mapped source objects (e.g. DemoVirtualMachine, DemoVirtualDisk) to
// build the parent-owned child graph (parent_graph.go:ensureParentOwnedChildGraphLayer); without this the
// list is Forbidden and the root Snapshot degrades to ChildrenSnapshotReady=False/SourceListForbidden.
//
// The verb is list ONLY (least privilege), because that is the entire access pattern: the planner does a
// one-shot r.Dynamic...List(namespace) per reconcile via a non-cached dynamic client. It never reads a
// single source by name (get) — it cannot know the names in advance, which is why it lists — and it never
// establishes a source informer (watch): the core controller's dynamic watches cover only the SNAPSHOT
// child GVKs (dynamic_watch.go), not sources, and sources are re-listed fresh on every reconcile. If a
// source-driven informer is ever added, add the watch verb here too. Read-only regardless: core discovers
// sources but never mutates them (creation/ownership is the domain SA's job). Like the snapshot GVRs,
// these resource names are domain-specific (from CSD), so they cannot live in the static,
// domain-agnostic core RBAC.
func buildCoreSourceReadRules(sourceGVRs []schema.GroupVersionResource) []rbacv1.PolicyRule {
	if len(sourceGVRs) == 0 {
		return nil
	}
	byGroup := make(map[string][]string)
	var groupOrder []string
	for _, gvr := range sourceGVRs {
		if _, ok := byGroup[gvr.Group]; !ok {
			groupOrder = append(groupOrder, gvr.Group)
		}
		byGroup[gvr.Group] = append(byGroup[gvr.Group], gvr.Resource)
	}
	sort.Strings(groupOrder)

	var rules []rbacv1.PolicyRule
	for _, g := range groupOrder {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{g},
			Resources: sortedUnique(byGroup[g]),
			Verbs:     []string{"list"},
		})
	}
	return rules
}

// coreManifestsSubresourceRules grants the DOMAIN SA get on core's per-CR /manifests-download subresource
// for each demo snapshot resource, so the domain apiserver can fetch each node's own (single-node) BASE
// manifests from the core apiserver (over the kube-apiserver aggregation layer). C9 made restore per-CR:
// the domain recurses children itself, fetching one node's base at a time, so it no longer reads core's
// (removed) whole-subtree /manifests.
func coreManifestsSubresourceRules(snapshotGVRs []schema.GroupVersionResource) []rbacv1.PolicyRule {
	if len(snapshotGVRs) == 0 {
		return nil
	}
	resources := make([]string, 0, len(snapshotGVRs))
	for _, gvr := range snapshotGVRs {
		resources = append(resources, gvr.Resource+"/manifests-download")
	}
	return []rbacv1.PolicyRule{{
		APIGroups: []string{consts.CoreSubresourcesGroup},
		Resources: sortedUnique(resources),
		Verbs:     []string{"get"},
	}}
}

// domainRestoreSubresourceRules grants the CORE SA get on the domain apiserver's
// /manifests-with-data-restoration subresource for each demo snapshot resource, so core can delegate the
// domain subtree restore. The subresource group is "subresources." + the snapshot's own API group.
func domainRestoreSubresourceRules(snapshotGVRs []schema.GroupVersionResource) []rbacv1.PolicyRule {
	if len(snapshotGVRs) == 0 {
		return nil
	}
	byGroup := make(map[string][]string)
	var groupOrder []string
	for _, gvr := range snapshotGVRs {
		subGroup := consts.DomainSubresourcesGroupPrefix + gvr.Group
		if _, ok := byGroup[subGroup]; !ok {
			groupOrder = append(groupOrder, subGroup)
		}
		byGroup[subGroup] = append(byGroup[subGroup], gvr.Resource+"/manifests-with-data-restoration")
	}
	sort.Strings(groupOrder)

	var rules []rbacv1.PolicyRule
	for _, g := range groupOrder {
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{g},
			Resources: sortedUnique(byGroup[g]),
			Verbs:     []string{"get"},
		})
	}
	return rules
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// applyDomainRBAC reconciles the two managed ClusterRoles + bindings of the split model:
//   - DomainClusterRoleName        bound to the DOMAIN SA  (domainRules)
//   - DomainCoreReadClusterRoleName bound to the CORE SA   (coreReadRules)
func applyDomainRBAC(ctx context.Context, cl ctrlclient.Client, domainRules, coreReadRules []rbacv1.PolicyRule) error {
	if err := applyManagedClusterRole(ctx, cl, consts.DomainClusterRoleName, domainRules, consts.DomainSAName); err != nil {
		return err
	}
	return applyManagedClusterRole(ctx, cl, consts.DomainCoreReadClusterRoleName, coreReadRules, consts.ControllerSAName)
}

// applyManagedClusterRole creates or updates a named ClusterRole and binds it to the given SA in the
// module namespace.
func applyManagedClusterRole(ctx context.Context, cl ctrlclient.Client, name string, rules []rbacv1.PolicyRule, saName string) error {
	if err := applyClusterRole(ctx, cl, name, rules); err != nil {
		return err
	}
	return applyClusterRoleBinding(ctx, cl, name, saName)
}

func applyClusterRole(ctx context.Context, cl ctrlclient.Client, name string, rules []rbacv1.PolicyRule) error {
	existing := new(rbacv1.ClusterRole)
	err := cl.Get(ctx, ctrlclient.ObjectKey{Name: name}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get ClusterRole %q: %w", name, err)
		}
		desired := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: moduleLabels(),
			},
			Rules: rules,
		}
		if createErr := cl.Create(ctx, desired); createErr != nil {
			return fmt.Errorf("create ClusterRole %q: %w", name, createErr)
		}
		return nil
	}
	base := existing.DeepCopy()
	existing.Rules = rules
	existing.Labels = moduleLabels()
	if patchErr := cl.Patch(ctx, existing, ctrlclient.MergeFrom(base)); patchErr != nil {
		return fmt.Errorf("patch ClusterRole %q: %w", name, patchErr)
	}
	return nil
}

func applyClusterRoleBinding(ctx context.Context, cl ctrlclient.Client, name, saName string) error {
	existing := new(rbacv1.ClusterRoleBinding)
	err := cl.Get(ctx, ctrlclient.ObjectKey{Name: name}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get ClusterRoleBinding %q: %w", name, err)
		}
		desired := desiredClusterRoleBinding(name, saName)
		if createErr := cl.Create(ctx, desired); createErr != nil {
			return fmt.Errorf("create ClusterRoleBinding %q: %w", name, createErr)
		}
		return nil
	}
	// roleRef is immutable; only subjects and labels can drift.
	base := existing.DeepCopy()
	existing.Subjects = subjectForSA(saName)
	existing.Labels = moduleLabels()
	if patchErr := cl.Patch(ctx, existing, ctrlclient.MergeFrom(base)); patchErr != nil {
		return fmt.Errorf("patch ClusterRoleBinding %q: %w", name, patchErr)
	}

	return nil
}

func desiredClusterRoleBinding(name, saName string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: moduleLabels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     name,
		},
		Subjects: subjectForSA(saName),
	}
}

func subjectForSA(saName string) []rbacv1.Subject {
	return []rbacv1.Subject{{
		Kind:      "ServiceAccount",
		Name:      saName,
		Namespace: consts.ModuleNamespace,
	}}
}

func moduleLabels() map[string]string {
	return map[string]string{
		"heritage": "deckhouse",
		"module":   consts.ModulePluralName,
	}
}

// desiredRBACReadyCondition builds the RBACReady condition value to write on a CSD.
func desiredRBACReadyCondition(generation int64, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               consts.CSDConditionRBACReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
	}
}

// patchCSDRBACReady performs a read-modify-update on the CSD status to set only
// the RBACReady condition, preserving Accepted and Ready (owned by the controller).
// Retries on conflict per the ADR ownership model.
func patchCSDRBACReady(ctx context.Context, cl ctrlclient.Client, name string, cond metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := new(v1alpha1.CustomSnapshotDefinition)
		if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: name}, fresh); err != nil {
			return err
		}
		existing := apimeta.FindStatusCondition(fresh.Status.Conditions, consts.CSDConditionRBACReady)
		if existing != nil &&
			existing.Status == cond.Status &&
			existing.Reason == cond.Reason &&
			existing.Message == cond.Message &&
			existing.ObservedGeneration == cond.ObservedGeneration {
			return nil
		}
		apimeta.SetStatusCondition(&fresh.Status.Conditions, cond)
		return cl.Status().Update(ctx, fresh)
	})
}
