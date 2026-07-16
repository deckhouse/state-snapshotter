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

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
)

// Legacy artifacts of the removed in-repo demo domain-controller. They were created OUT-OF-BAND from
// helm (this hook applied the ClusterRole/ClusterRoleBinding; the tls-certificate hook owned the Secret),
// so a module upgrade does not garbage-collect them: without an explicit delete they would linger forever,
// and the ClusterRoleBinding would silently re-arm if any pod ever got a ServiceAccount named
// "domain-controller" in the module namespace. The names are intentionally NOT in consts anymore — they
// must never be referenced by anything but this cleanup.
const (
	// legacyDomainClusterRoleName was consts.DomainClusterRoleName: dynamic demo source/snapshot GVR
	// rights + core-subresource gets, bound to the removed domain-controller SA.
	legacyDomainClusterRoleName = "d8:state-snapshotter:controller:domain"
	// legacyDomainTLSSecretName was consts.DomainAPIServerSecretName: the removed domain aggregated
	// apiserver's serving certificate, managed by the removed 020-apiserver-certs registration.
	legacyDomainTLSSecretName = "state-snapshotter-domain-tls-certs"
)

// cleanupLegacyDomainControllerArtifacts deletes the hook-managed leftovers of the removed demo
// domain-controller (ClusterRole + ClusterRoleBinding "d8:state-snapshotter:controller:domain" and the
// domain TLS Secret). Idempotent: NotFound is success, so steady-state cost is three cheap DELETEs that
// no-op. Runs on every reconcile rather than as a one-shot migration so a cleanup missed during one
// converge (transient apiserver error) is retried on the next.
func cleanupLegacyDomainControllerArtifacts(ctx context.Context, cl ctrlclient.Client) error {
	objects := []ctrlclient.Object{
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: legacyDomainClusterRoleName}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: legacyDomainClusterRoleName}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: legacyDomainTLSSecretName, Namespace: consts.ModuleNamespace}},
	}
	for _, obj := range objects {
		if err := cl.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete legacy domain-controller object %T %s/%s: %w",
				obj, obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}
