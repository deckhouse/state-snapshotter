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

package consts

const (
	// ModuleName is the name of the state-snapshotter module (camelCase for Helm values)
	ModuleName = "stateSnapshotter"

	// ModuleNamespace is the default namespace for the module
	ModuleNamespace = "d8-state-snapshotter"

	// APIServerCertCN is the Common Name for API server certificate (used for APIService)
	// Must match the Service name in templates/controller/service.yaml
	// This is used for SAN (Subject Alternative Names) in the certificate
	APIServerCertCN = "controller"

	// APIServerSecretName is the name of the Kubernetes Secret containing TLS certificates
	// This can be different from APIServerCertCN for better naming clarity
	APIServerSecretName = "state-snapshotter-tls-certs"

	ModulePluralName = "state-snapshotter"

	// WebhookCertCN is the Common Name for webhook certificate
	WebhookCertCN = "webhooks"

	// WebhookSecretName is the name of the Kubernetes Secret containing webhook TLS certificates
	WebhookSecretName = "webhooks-https-certs"

	// ControllerSAName is the ServiceAccount name of the state-snapshotter controller.
	ControllerSAName = "controller"

	// DomainClusterRoleName is the single aggregated ClusterRole managed by the 030-domain-rbac hook
	DomainClusterRoleName = "d8:state-snapshotter:controller:domain"

	// CSD condition types referenced by the domain-RBAC hook.
	// Accepted is owned by the CSD reconciler; RBACReady is owned exclusively by this hook.
	CSDConditionAccepted  = "Accepted"
	CSDConditionRBACReady = "RBACReady"

	// RBACReady condition reasons per ADR snapshot-rework/2026-01-23-unified-snapshots-registry.md §2.
	RBACReadyReasonPending     = "Pending"     // snapshot GVR not yet resolvable via discovery
	RBACReadyReasonApplyFailed = "ApplyFailed" // ClusterRole/Binding creation or update failed
	RBACReadyReasonApplied     = "Applied"     // RBAC successfully applied for all snapshot GVRs
)
