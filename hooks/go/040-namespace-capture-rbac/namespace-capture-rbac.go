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

// Package namespace_capture_rbac implements the 040-namespace-capture-rbac hook: a level-based reconciler
// that grants the state-snapshotter controller SA broad read access to a namespace ONLY while that
// namespace hosts a Snapshot still capturing the live namespace. It maintains the invariant:
//
//	a managed RoleBinding (to the wildcard capture ClusterRole) exists in namespace N
//	  <=> N hosts at least one Snapshot with needsCaptureRBAC == true.
//
// The hook runs under the privileged deckhouse SA (see Phase 5.3), so it needs no extra RBAC provisioning
// of its own, and the privilege-escalation guard for binding the wildcard role is satisfied automatically.
package namespace_capture_rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/deckhouse/module-sdk/pkg"
	sdkk8s "github.com/deckhouse/module-sdk/pkg/dependency/k8s"
	"github.com/deckhouse/module-sdk/pkg/registry"
	"github.com/deckhouse/module-sdk/pkg/utils/ptr"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	modulePluralName = consts.ModulePluralName

	// captureClusterRoleName must match the static wildcard ClusterRole in
	// templates/controller/rbac-for-us.yaml (d8:<chart>:capture-namespace).
	captureClusterRoleName = "d8:" + consts.ModulePluralName + ":capture-namespace"

	// captureRoleBindingName is the fixed, per-namespace name of the managed RoleBinding (one per namespace).
	captureRoleBindingName = "d8-" + consts.ModulePluralName + "-capture"

	// captureRBACManagedLabelKey/Value mark RoleBindings owned by THIS hook. The hook lists/deletes only
	// objects carrying this marker, so it never touches Helm-managed module RoleBindings that share the
	// heritage/module labels.
	captureRBACManagedLabelKey   = "state-snapshotter.deckhouse.io/capture-rbac"
	captureRBACManagedLabelValue = "true"
)

var _ = registry.RegisterFunc(
	&pkg.HookConfig{
		Kubernetes: []pkg.KubernetesConfig{
			{
				Name:                         "snapshots",
				APIVersion:                   "storage.deckhouse.io/v1alpha1",
				Kind:                         "Snapshot",
				ExecuteHookOnSynchronization: ptr.Bool(true),
				ExecuteHookOnEvents:          ptr.Bool(true),
			},
			{
				// Watch only our own managed capture RoleBindings so manual deletion (self-heal) or drift
				// re-triggers reconciliation without reacting to unrelated RoleBindings.
				Name:                         "managed-capture-rolebindings",
				APIVersion:                   "rbac.authorization.k8s.io/v1",
				Kind:                         "RoleBinding",
				LabelSelector:                &metav1.LabelSelector{MatchLabels: map[string]string{captureRBACManagedLabelKey: captureRBACManagedLabelValue}},
				ExecuteHookOnSynchronization: ptr.Bool(true),
				ExecuteHookOnEvents:          ptr.Bool(true),
			},
		},
		Queue: "modules/" + consts.ModuleName,
	},
	reconcileNamespaceCaptureRBAC,
)

// reconcileNamespaceCaptureRBAC is a full, level-based reconcile (independent of which event woke it): it
// recomputes the desired set of capture namespaces from all Snapshots, lists the hook-managed
// RoleBindings, and converges the two (create/update for desired, delete for orphans). Being level-based
// makes it self-healing against manual RoleBinding deletion or controller downtime.
func reconcileNamespaceCaptureRBAC(ctx context.Context, input *pkg.HookInput) error {
	cl := input.DC.MustGetK8sClient(sdkk8s.WithSchemeBuilder(storagev1alpha1.SchemeBuilder))

	snaps := new(storagev1alpha1.SnapshotList)
	if err := cl.List(ctx, snaps); err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	desired := namespacesNeedingCaptureRBAC(snaps.Items)

	managed := new(rbacv1.RoleBindingList)
	if err := cl.List(ctx, managed, ctrlclient.MatchingLabels{captureRBACManagedLabelKey: captureRBACManagedLabelValue}); err != nil {
		return fmt.Errorf("list managed capture rolebindings: %w", err)
	}
	existing := make(map[string]struct{}, len(managed.Items))
	for i := range managed.Items {
		rb := &managed.Items[i]
		// Safety: only ever manage our exact fixed-name binding.
		if rb.Name != captureRoleBindingName {
			continue
		}
		existing[rb.Namespace] = struct{}{}
	}

	var errs []error
	// Create/update in every desired namespace (CreateOrUpdate is idempotent and repairs subject/label drift).
	for ns := range desired {
		if err := applyCaptureRoleBinding(ctx, cl, ns); err != nil {
			errs = append(errs, fmt.Errorf("ensure capture rolebinding in namespace %q: %w", ns, err))
		}
	}
	// Delete managed bindings in namespaces that no longer need capture RBAC (least privilege restored).
	for ns := range existing {
		if _, ok := desired[ns]; ok {
			continue
		}
		if err := deleteCaptureRoleBinding(ctx, cl, ns); err != nil {
			errs = append(errs, fmt.Errorf("delete capture rolebinding in namespace %q: %w", ns, err))
		}
	}

	return errors.Join(errs...)
}

// applyCaptureRoleBinding creates or updates the managed RoleBinding in the given namespace, pointing the
// controller SA at the wildcard capture ClusterRole. roleRef is immutable, so on update only subjects and
// labels are reconciled.
func applyCaptureRoleBinding(ctx context.Context, cl ctrlclient.Client, namespace string) error {
	existing := new(rbacv1.RoleBinding)
	err := cl.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: captureRoleBindingName}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get RoleBinding: %w", err)
		}
		if createErr := cl.Create(ctx, desiredCaptureRoleBinding(namespace)); createErr != nil {
			return fmt.Errorf("create RoleBinding: %w", createErr)
		}
		return nil
	}
	base := existing.DeepCopy()
	existing.Subjects = captureSubjects()
	existing.Labels = captureRoleBindingLabels()
	if patchErr := cl.Patch(ctx, existing, ctrlclient.MergeFrom(base)); patchErr != nil {
		return fmt.Errorf("patch RoleBinding: %w", patchErr)
	}
	return nil
}

// deleteCaptureRoleBinding removes the managed RoleBinding from the given namespace (NotFound is a no-op).
func deleteCaptureRoleBinding(ctx context.Context, cl ctrlclient.Client, namespace string) error {
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: captureRoleBindingName}}
	if err := cl.Delete(ctx, rb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete RoleBinding: %w", err)
	}
	return nil
}

func desiredCaptureRoleBinding(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      captureRoleBindingName,
			Labels:    captureRoleBindingLabels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     captureClusterRoleName,
		},
		Subjects: captureSubjects(),
	}
}

func captureSubjects() []rbacv1.Subject {
	return []rbacv1.Subject{{
		Kind:      "ServiceAccount",
		Name:      consts.ControllerSAName,
		Namespace: consts.ModuleNamespace,
	}}
}
