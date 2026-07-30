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

	// ControllerSAName is the ServiceAccount name of the state-snapshotter (core) controller.
	ControllerSAName = "controller"

	// WebhooksSAName is the ServiceAccount name of the state-snapshotter webhooks pod (must match
	// templates/webhooks/rbac-for-us.yaml). The 040-namespace-capture-rbac hook adds it as a second
	// subject on the transient per-namespace capture RoleBinding so the MCR-validation webhook can resolve
	// arbitrary (non-allowlisted) namespaced CR targets via a dynamic Get during the capture window.
	// Independently of that window, DomainWebhookReadClusterRoleName grants it a standing get on the
	// CSD-registered domain SOURCE GVRs (see below).
	WebhooksSAName = "webhooks"

	// DomainWebhookReadClusterRoleName is the ClusterRole the 030-domain-rbac hook binds to the WEBHOOKS SA:
	// get (only) on the dynamic domain SOURCE GVRs. The MCR-validation webhook verifies that each
	// spec.targets[] entry exists in the MCR namespace with one named dynamic Get per target
	// (mcrValidator.findResourceNamespace) — no list, no watch. The transient capture RoleBinding from
	// 040-namespace-capture-rbac covers this only while a namespace is actively capturing, and MCR admission
	// is not ordered against that RoleBinding, so without a standing grant the Get can be denied. A denied
	// Get is NOT evidence of absence: the webhook fails closed with an explicit RBAC diagnostic rather than
	// reporting a misleading "target not found". Like the other domain roles, these resource names come from
	// CustomSnapshotDefinition, so they cannot live in the static, domain-agnostic webhook RBAC.
	DomainWebhookReadClusterRoleName = "d8:state-snapshotter:webhooks:domain-read"

	// DomainCoreReadClusterRoleName is the ClusterRole the 030-domain-rbac hook binds to the CORE SA for
	// the dynamic demo GVRs: read + create + patch + status-write on the snapshot GVRs (the core
	// SnapshotReconciler is the parent-graph planner — it creates one child snapshot per source object and
	// patches its ownerRef back to the root Snapshot), get + list on the source GVRs (list to enumerate
	// sources during planning, get to capture each target's manifest), plus
	// get on the domain /manifests-with-data-restoration aggregated subresource (so core can delegate
	// restore). These names are domain-specific (from CSD), so they cannot live in the static,
	// domain-agnostic core RBAC.
	DomainCoreReadClusterRoleName = "d8:state-snapshotter:controller:domain-read"

	// DataExportModuleNamespace is the namespace of the storage-foundation module, whose DataExport
	// controller resolves snapshot exports generically (no domain types compiled in). The DataExport/
	// DataImport feature was absorbed from the former storage-volume-data-manager module.
	DataExportModuleNamespace = "d8-storage-foundation"

	// DataExportControllerSAName is the ServiceAccount name of the storage-foundation DataExport/DataImport
	// reconciler (data-manager-controller). It runs in DataExportModuleNamespace.
	DataExportControllerSAName = "data-manager-controller"

	// DomainDataExportReadClusterRoleName is the ClusterRole the 030-domain-rbac hook binds to the
	// storage-foundation DataExport controller SA: read on the dynamic demo snapshot GVRs. The
	// DataExport controller resolves a snapshot export by GETting the snapshot leaf to read
	// status.boundSnapshotContentName (then follows it to the cluster-scoped SnapshotContent). These names
	// are domain-specific (from CSD), so they cannot live in that module's static, domain-agnostic RBAC.
	DomainDataExportReadClusterRoleName = "d8:state-snapshotter:data-export:domain-read"

	// DomainSubresourcesGroupPrefix is prepended to a domain snapshot's API group to address its
	// aggregated subresources group (e.g. "sds-unified-snapshots-poc.deckhouse.io" ->
	// "subresources.sds-unified-snapshots-poc.deckhouse.io"). Keep in sync with internal/api and
	// internal/domainapi.
	DomainSubresourcesGroupPrefix = "subresources."

	// CSD condition types referenced by the domain-RBAC hook.
	// Accepted is owned by the CSD reconciler; AccessGranted is owned exclusively by this hook.
	CSDConditionAccepted      = "Accepted"
	CSDConditionAccessGranted = "AccessGranted"

	// AccessGranted condition reasons per ADR (architecture-decision-records: dkp/storage/state-snapshotter/old2/2026-01-23-unified-snapshots-registry.md) §2.
	AccessGrantedReasonPending     = "Pending"     // snapshot GVR not yet resolvable via discovery
	AccessGrantedReasonApplyFailed = "ApplyFailed" // ClusterRole/Binding creation or update failed
	AccessGrantedReasonApplied     = "Applied"     // RBAC successfully applied for all snapshot GVRs
)
