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
		sort.Strings(entry.sources)
		sort.Strings(entry.snapshots)

		if len(entry.sources) > 0 {
			// PERMANENT: the general controller lists source resources (e.g. DemoVirtualDisk,
			// DemoVirtualMachine) to discover what child snapshots to create.
			// Forbidden → sourceListForbiddenError: graph degrades, not silently empty.
			// Reference: parent_graph.go:ensureParentOwnedChildGraphLayer.
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

// applyDomainClusterRole creates or updates the domain ClusterRole and its binding.
func applyDomainClusterRole(ctx context.Context, cl ctrlclient.Client, rules []rbacv1.PolicyRule) error {
	if err := applyClusterRole(ctx, cl, rules); err != nil {
		return err
	}
	return applyClusterRoleBinding(ctx, cl)
}

func applyClusterRole(ctx context.Context, cl ctrlclient.Client, rules []rbacv1.PolicyRule) error {
	existing := new(rbacv1.ClusterRole)
	err := cl.Get(ctx, ctrlclient.ObjectKey{Name: consts.DomainClusterRoleName}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get ClusterRole %q: %w", consts.DomainClusterRoleName, err)
		}
		desired := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name:   consts.DomainClusterRoleName,
				Labels: moduleLabels(),
			},
			Rules: rules,
		}
		if createErr := cl.Create(ctx, desired); createErr != nil {
			return fmt.Errorf("create ClusterRole %q: %w", consts.DomainClusterRoleName, createErr)
		}
		return nil
	}
	base := existing.DeepCopy()
	existing.Rules = rules
	existing.Labels = moduleLabels()
	if patchErr := cl.Patch(ctx, existing, ctrlclient.MergeFrom(base)); patchErr != nil {
		return fmt.Errorf("patch ClusterRole %q: %w", consts.DomainClusterRoleName, patchErr)
	}
	return nil
}

func applyClusterRoleBinding(ctx context.Context, cl ctrlclient.Client) error {
	existing := new(rbacv1.ClusterRoleBinding)
	err := cl.Get(ctx, ctrlclient.ObjectKey{Name: consts.DomainClusterRoleName}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get ClusterRoleBinding %q: %w", consts.DomainClusterRoleName, err)
		}
		desired := desiredClusterRoleBinding()
		if createErr := cl.Create(ctx, desired); createErr != nil {
			return fmt.Errorf("create ClusterRoleBinding %q: %w", consts.DomainClusterRoleName, createErr)
		}
		return nil
	}
	// roleRef is immutable; only subjects and labels can drift.
	base := existing.DeepCopy()
	existing.Subjects = desiredSubjects()
	existing.Labels = moduleLabels()
	if patchErr := cl.Patch(ctx, existing, ctrlclient.MergeFrom(base)); patchErr != nil {
		return fmt.Errorf("patch ClusterRoleBinding %q: %w", consts.DomainClusterRoleName, patchErr)
	}

	return nil
}

func desiredClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   consts.DomainClusterRoleName,
			Labels: moduleLabels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     consts.DomainClusterRoleName,
		},
		Subjects: desiredSubjects(),
	}
}

func desiredSubjects() []rbacv1.Subject {
	return []rbacv1.Subject{{
		Kind:      "ServiceAccount",
		Name:      consts.ControllerSAName,
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
