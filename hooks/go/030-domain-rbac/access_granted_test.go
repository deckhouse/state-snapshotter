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
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/deckhouse/pkg/log"
	"github.com/deckhouse/module-sdk/pkg"
	"github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
)

// The demo domain these fixtures describe: one source kind and the snapshot kind mapped onto it.
const (
	agDomainGroup   = "sds-unified-snapshots-poc.deckhouse.io"
	agDomainVersion = "v1alpha1"
	agDomainAPI     = agDomainGroup + "/" + agDomainVersion

	agSourceKind       = "DemoVirtualDisk"
	agSourceResource   = "demovirtualdisks"
	agSnapshotKind     = "DemoVirtualDiskSnapshot"
	agSnapshotResource = "demovirtualdisksnapshots"
)

// stubK8sClient adapts a controller-runtime client to the module-sdk KubernetesClient interface, which is
// client.Client plus Dynamic(). The hook never reaches for the dynamic client, so it stays nil: that is not
// a shortcut hiding a call, it fails loudly if one is ever added.
type stubK8sClient struct {
	ctrlclient.Client
}

func (stubK8sClient) Dynamic() dynamic.Interface { return nil }

// stubDependencyContainer serves exactly one dependency — the Kubernetes client. The embedded interface is
// nil on purpose: any other dependency the hook starts to use panics here instead of silently returning a
// zero value, so this double cannot drift into pretending to support something it does not.
type stubDependencyContainer struct {
	pkg.DependencyContainer
	client pkg.KubernetesClient
}

func (d stubDependencyContainer) MustGetK8sClient(_ ...pkg.KubernetesOption) pkg.KubernetesClient {
	return d.client
}

// agScheme carries the CSD type the hook lists and status-updates, the RBAC types it writes, and core (the
// legacy domain TLS Secret the cleanup step deletes).
func agScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		rbacv1.AddToScheme,
		corev1.AddToScheme,
		v1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("build test scheme: %v", err)
		}
	}
	return scheme
}

// agRESTMapper maps the demo kinds listed in known. Kinds left out stay unresolvable, which reproduces
// discovery lag (the CRD is not served yet) without a live API server.
func agRESTMapper(known ...string) apimeta.RESTMapper {
	gv := schema.GroupVersion{Group: agDomainGroup, Version: agDomainVersion}
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	for _, kind := range known {
		mapper.Add(gv.WithKind(kind), apimeta.RESTScopeNamespace)
	}
	return mapper
}

// acceptedCSD builds an eligible CSD: Accepted=True observed at the current generation, which is what the
// hook requires before it grants anything for that definition.
func acceptedCSD(name string) *v1alpha1.CustomSnapshotDefinition {
	def := &v1alpha1.CustomSnapshotDefinition{}
	def.Name = name
	def.Generation = 3
	def.Spec.APIVersion = agDomainAPI
	def.Spec.Kind = agSnapshotKind
	def.Spec.Source = v1alpha1.SnapshotGVKRef{APIVersion: agDomainAPI, Kind: agSourceKind}
	def.Status.Conditions = []metav1.Condition{{
		Type:               consts.CSDConditionAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		ObservedGeneration: def.Generation,
		LastTransitionTime: metav1.Now(),
	}}
	return def
}

// agHookInput wires the reconcile function to a client without a live cluster.
func agHookInput(cl ctrlclient.Client) *pkg.HookInput {
	return &pkg.HookInput{
		DC:     stubDependencyContainer{client: stubK8sClient{Client: cl}},
		Logger: log.NewNop(),
	}
}

// agAccessGranted reads back the condition this hook owns on a CSD.
func agAccessGranted(t *testing.T, cl ctrlclient.Client, name string) *metav1.Condition {
	t.Helper()
	fresh := new(v1alpha1.CustomSnapshotDefinition)
	if err := cl.Get(context.Background(), ctrlclient.ObjectKey{Name: name}, fresh); err != nil {
		t.Fatalf("get CSD %q: %v", name, err)
	}
	return apimeta.FindStatusCondition(fresh.Status.Conditions, consts.CSDConditionAccessGranted)
}

// clusterRoleGrantsDomainResource reports whether the role grants at least one verb on the demo domain's
// named resource.
func clusterRoleGrantsDomainResource(cr *rbacv1.ClusterRole, resource string) bool {
	for _, rule := range cr.Rules {
		if len(rule.Verbs) > 0 &&
			slices.Contains(rule.APIGroups, agDomainGroup) &&
			slices.Contains(rule.Resources, resource) {
			return true
		}
	}
	return false
}

// AccessGranted=True/Applied promises core-side access is in place for EVERY consumer of the domain's
// resources, and there are three: the core controller SA, the webhooks SA (target validation at admission)
// and the storage-foundation DataExport SA. The condition is one flag over all three, so a grant lost for a
// single consumer is invisible in the condition itself — which is what this test guards.
//
// The defect it catches: the reconcile stops feeding one of the three rule sets (for example the webhook
// source rules are no longer built, or no longer passed on). That role is then reconciled empty while the
// condition still reports Applied, and MCR target validation starts failing at admission outside the
// transient capture window. Confirmed by mutation: passing nil in place of the webhook rules turns this test
// red and leaves the rest of the package green.
func TestReconcileDomainRBACAppliedAttestsAllThreeGrantees(t *testing.T) {
	ctx := context.Background()
	def := acceptedCSD("demo-disk")
	cl := fake.NewClientBuilder().
		WithScheme(agScheme(t)).
		WithRESTMapper(agRESTMapper(agSourceKind, agSnapshotKind)).
		WithStatusSubresource(&v1alpha1.CustomSnapshotDefinition{}).
		WithObjects(def).
		Build()

	if err := reconcileDomainRBAC(ctx, agHookInput(cl)); err != nil {
		t.Fatalf("reconcileDomainRBAC: %v", err)
	}

	cond := agAccessGranted(t, cl, def.Name)
	if cond == nil {
		t.Fatalf("no %s condition on the eligible CSD", consts.CSDConditionAccessGranted)
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != consts.AccessGrantedReasonApplied {
		t.Fatalf("%s = %s/%s (%s), want True/%s", consts.CSDConditionAccessGranted,
			cond.Status, cond.Reason, cond.Message, consts.AccessGrantedReasonApplied)
	}
	if cond.ObservedGeneration != def.Generation {
		t.Errorf("observedGeneration = %d, want %d — the condition must be pinned to the spec it attests",
			cond.ObservedGeneration, def.Generation)
	}

	// One row per grantee: the ClusterRole the hook reconciles, the SA bound to it, and the domain resource
	// that role has to mention. Sources are what the core planner lists and the webhook Gets at admission;
	// snapshots are what the core planner creates and the DataExport controller reads.
	//
	// The three rows are the whole point: dropping any one of them makes the test pass with that consumer
	// ungranted, which is the failure this test exists to prevent.
	for _, want := range []struct {
		grantee      string
		role         string
		saName       string
		saNamespace  string
		wantResource string
	}{
		{"core controller", consts.DomainCoreReadClusterRoleName, consts.ControllerSAName, consts.ModuleNamespace, agSourceResource},
		{"webhooks", consts.DomainWebhookReadClusterRoleName, consts.WebhooksSAName, consts.ModuleNamespace, agSourceResource},
		{"data export", consts.DomainDataExportReadClusterRoleName, consts.DataExportControllerSAName, consts.DataExportModuleNamespace, agSnapshotResource},
	} {
		cr := new(rbacv1.ClusterRole)
		if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: want.role}, cr); err != nil {
			t.Errorf("%s: get ClusterRole %q: %v", want.grantee, want.role, err)
			continue
		}
		if !clusterRoleGrantsDomainResource(cr, want.wantResource) {
			t.Errorf("%s: ClusterRole %q grants nothing on %s/%s — this consumer lost its grant while the condition still reports %s; rules=%#v",
				want.grantee, want.role, agDomainGroup, want.wantResource, consts.AccessGrantedReasonApplied, cr.Rules)
		}

		crb := new(rbacv1.ClusterRoleBinding)
		if err := cl.Get(ctx, ctrlclient.ObjectKey{Name: want.role}, crb); err != nil {
			t.Errorf("%s: get ClusterRoleBinding %q: %v", want.grantee, want.role, err)
			continue
		}
		if len(crb.Subjects) != 1 ||
			crb.Subjects[0].Name != want.saName ||
			crb.Subjects[0].Namespace != want.saNamespace {
			t.Errorf("%s: ClusterRoleBinding %q subjects = %#v, want the single ServiceAccount %s/%s",
				want.grantee, want.role, crb.Subjects, want.saNamespace, want.saName)
		}
	}

	// The rows above are checked one by one, so deleting a row would quietly narrow the test. This closes
	// that hole from the other side: the set of ClusterRoles the hook manages must be exactly the three
	// named above — no role dropped, and none added without a row here to describe who it is for.
	managed := new(rbacv1.ClusterRoleList)
	if err := cl.List(ctx, managed, ctrlclient.MatchingLabels(moduleLabels())); err != nil {
		t.Fatalf("list hook-managed ClusterRoles: %v", err)
	}
	gotRoles := make([]string, 0, len(managed.Items))
	for i := range managed.Items {
		gotRoles = append(gotRoles, managed.Items[i].Name)
	}
	slices.Sort(gotRoles)
	wantRoles := []string{
		consts.DomainCoreReadClusterRoleName,
		consts.DomainWebhookReadClusterRoleName,
		consts.DomainDataExportReadClusterRoleName,
	}
	slices.Sort(wantRoles)
	if !slices.Equal(gotRoles, wantRoles) {
		t.Errorf("hook-managed ClusterRoles = %v (%d), want exactly %v (%d)",
			gotRoles, len(gotRoles), wantRoles, len(wantRoles))
	}
}

// A CSD accepted before its kinds are served by discovery must NOT be reported as granted: there is nothing
// to write a rule about yet, so the honest answer is AccessGranted=False/Pending plus a retry once discovery
// catches up.
//
// The defect it catches: an unresolvable GVR is read as "no resources to grant, therefore done" and the CSD
// flips to Applied. Consumers that gate on AccessGranted then start work against a role that grants nothing,
// and the failure surfaces far from its cause. Confirmed by mutation: removing the pending branch from the
// outcome switch turns this test red.
func TestReconcileDomainRBACPendingWhileSnapshotGVRUnresolved(t *testing.T) {
	ctx := context.Background()
	def := acceptedCSD("demo-disk-early")
	cl := fake.NewClientBuilder().
		WithScheme(agScheme(t)).
		// The source kind resolves, the snapshot kind does not: the domain CRDs are still being installed.
		WithRESTMapper(agRESTMapper(agSourceKind)).
		WithStatusSubresource(&v1alpha1.CustomSnapshotDefinition{}).
		WithObjects(def).
		Build()

	if err := reconcileDomainRBAC(ctx, agHookInput(cl)); err != nil {
		t.Fatalf("reconcileDomainRBAC must not fail while discovery lags: %v", err)
	}

	cond := agAccessGranted(t, cl, def.Name)
	if cond == nil {
		t.Fatalf("no %s condition on the pending CSD", consts.CSDConditionAccessGranted)
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != consts.AccessGrantedReasonPending {
		t.Fatalf("%s = %s/%s (%s), want False/%s", consts.CSDConditionAccessGranted,
			cond.Status, cond.Reason, cond.Message, consts.AccessGrantedReasonPending)
	}
	if !strings.Contains(cond.Message, agSnapshotKind) {
		t.Errorf("message = %q, want it to name the kind that did not resolve (%s)", cond.Message, agSnapshotKind)
	}
}

// When the RBAC write itself is rejected, the CSD has to say so AND the hook has to fail, so the queue
// retries. Reporting the rejection only in the log would leave the CSD claiming access that does not exist.
//
// The defect it catches: the apply error is swallowed — either the condition still reads Applied, or the
// reconcile returns nil and the queue never retries, so one rejected write strands the domain without access
// indefinitely. Confirmed by mutation: returning nil instead of the apply error turns this test red.
func TestReconcileDomainRBACApplyFailedWhenRBACWriteRejected(t *testing.T) {
	ctx := context.Background()
	def := acceptedCSD("demo-disk-denied")
	const rejection = "clusterroles.rbac.authorization.k8s.io is forbidden"
	cl := fake.NewClientBuilder().
		WithScheme(agScheme(t)).
		WithRESTMapper(agRESTMapper(agSourceKind, agSnapshotKind)).
		WithStatusSubresource(&v1alpha1.CustomSnapshotDefinition{}).
		WithObjects(def).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
				if _, isClusterRole := obj.(*rbacv1.ClusterRole); isClusterRole {
					return errors.New(rejection)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	err := reconcileDomainRBAC(ctx, agHookInput(cl))
	if err == nil {
		t.Fatal("reconcileDomainRBAC returned nil after the ClusterRole write was rejected; the queue would never retry")
	}
	if !strings.Contains(err.Error(), rejection) {
		t.Errorf("returned error = %v, want it to carry the rejection %q", err, rejection)
	}

	cond := agAccessGranted(t, cl, def.Name)
	if cond == nil {
		t.Fatalf("no %s condition after the failed apply", consts.CSDConditionAccessGranted)
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != consts.AccessGrantedReasonApplyFailed {
		t.Fatalf("%s = %s/%s (%s), want False/%s", consts.CSDConditionAccessGranted,
			cond.Status, cond.Reason, cond.Message, consts.AccessGrantedReasonApplyFailed)
	}
	if !strings.Contains(cond.Message, rejection) {
		t.Errorf("message = %q, want it to carry the rejection %q so an operator can see why access is missing",
			cond.Message, rejection)
	}
}
