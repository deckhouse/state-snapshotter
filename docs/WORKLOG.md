# Work Log

Chronological log of notable refactors. Newest wave at the bottom.

## Wave 3 — Status-model redesign (Ready + captureState + top-level source)

- **Refactor** Made `Ready` the sole user-facing condition on `Snapshot`/`SnapshotContent`. Internalized the former `PlanningReady`/`ManifestsArchived` conditions into `status.captureState` (`commonController` + `domainSpecificController`) and the core-internal `status.subtreeManifestsPersisted` bool latch.
- **Add** `SnapshotCapturePhase` enum (`Planning|Planned|Finished|Failed`) as the domain-owned lifecycle on `status.captureState.domainSpecificController.phase`; replaced the domain `Ready`/planning conditions.
- **Add** `spec.mode` enum (`Capture|Import|StaticBind`) on `Snapshot`, replacing the `spec.source.import: {}` marker for Deckhouse CRDs. The CSI-shaped extended VolumeSnapshot keeps the kind-aware `spec.source.import: {}` marker. Added CEL: `snapshotContentName` required iff `mode: StaticBind`.
- **Add** top-level `status.snapshotSource` (`SnapshotSourceObjectRef`); removed `status.sourceUID` and the source-ref annotation. `PublishSourceUID` verb replaced by `PublishSnapshotSource`.
- **Remove** `Snapshot.status.observedGeneration` (spec is immutable, so it carried no signal).
- **Remove** `Snapshot.status.manifestCaptureRequestName` / `volumeCaptureRequestName` from the public status; the root MCR/VCR names are core-internal execution handles (deterministic name + ownerRef + label).
- **Refactor** SDK verbs: `MarkPlanningReady`→`MarkPlanned`; added `ConfirmConsistent`; `MarkPlanningFailed`/`MarkNotReady`→`Fail`/`Reject`; added `CoreCaptureOutcome` tri-state helper (`Capturing|Captured|Failed`).
- **Refactor** Core RBAC drop hook now keys on `captureState.commonController.manifestCaptured` + `IsReasonTerminal(Ready.reason)` instead of the removed `ManifestsArchived` condition.
- **Rename** Unified finalizer prefixes to `state-snapshotter.deckhouse.io/` (removed the legacy `snapshot.deckhouse.io/*` and `snapshot.finalizers.deckhouse.io` prefixes).
- **Remove** `labelSnapshotUID` from the orphan-PVC VolumeSnapshot; kept the const + MCR label guard.
- **Update** `CustomSnapshotDefinition` printcolumns: surfaced `AccessGranted` + `Ready` next to `Accepted`; documented `Ready = Accepted && AccessGranted`.
- **Update** d8-cli: consume `spec.mode` instead of `source.import`; build `snapshotSource` from top-level `status.snapshotSource`; read only `Ready`; synced local API types; removed `internal/snapshot/source/source_ref.go`.
- **Update** Both ADRs synced to the field model (committed by maintainer).

### Verification (w3-verify)

- Grep invariants across state-snapshotter + d8-cli == 0: `ConditionPlanningReady`, `ConditionManifestsArchived` (user-facing), `NotReadySpec`, `sourceUID`, `PublishSourceUID`, `ReasonPriorityLayerPending`/`PriorityLayerPending`/`priorityLayer`; old finalizer prefixes (`snapshot.deckhouse.io/parent-protect`, `snapshot.deckhouse.io/artifact-protect`, `snapshot.finalizers.deckhouse.io`) == 0. Remaining `spec.source.import` references are the intentional kind-aware CSI-VolumeSnapshot import marker only.
- Build: all state-snapshotter modules (`api`, `pkg/snapshotsdk`, `images/{state-snapshotter-controller,domain-controller,webhooks}`, `hooks/go`) and d8-cli `internal/snapshot/...` build cleanly.
- Unit tests: all state-snapshotter modules and d8-cli snapshot packages pass.
- Integration: the 3 static-bind failures were wave3-introduced (new `mode: StaticBind` CEL) and are fixed by setting `Mode` in those fixtures. The remaining integration failures were confirmed pre-existing on the base commit (`958526c`) via a base-worktree run and are unrelated to wave3 (content-driven fixtures referencing a non-existent owning Snapshot; child terminal-failure bridge fragility).

## Wave 4A — Resource selection: exclude veto + durable excludedRefs

- **Add** `ExcludeLabelKey` (`state-snapshotter.deckhouse.io/exclude`) as the single exported source of truth for the absolute snapshot veto (key-presence only, value ignored — Velero convention). Lives in `api/storage/v1alpha1/labels.go`, re-exported from the SDK (`snapshotsdk.ExcludeLabelKey`).
- **Update** `Snapshot.ResolveResourceSelector` to always AND an `ExcludeLabelKey DoesNotExist` requirement onto `spec.resourceSelector` (nil/empty selector included), so a vetoed object is dropped from expansion and manifest legs at every level.
- **Add** `ExcludedObjectRef` api type (`{apiVersion,kind,name}`, the shadow of `SnapshotChildRef`) plus three `excludedRefs` fields (Model B'): domain input `status.captureState.domainSpecificController.excludedRefs` (written without omitempty — `[]` vs absent is meaningful), the durable aggregate `SnapshotContent.status.excludedRefs` (cluster-scoped truth), and the top-level mirror `status.excludedRefs` on `Snapshot`/domain CRs (user-facing audit). Regenerated deepcopy + CRDs.
- **Add** SDK exclude helpers `IsExcluded` / `PartitionExcluded`; extended `EnsureChildren(ctx, t, desired, excluded)` to publish both kept children and excluded source refs into `DomainCaptureState.ExcludedRefs`; nil→`[]` coercion in the demo adapters.
- **Update** demo VM snapshot planner (`planDemoVirtualMachineChildren`) to partition owned disks via `PartitionExcluded`: kept disks become child snapshots, vetoed disks become `excludedRefs`.
- **Add** core durable aggregation: the SnapshotContent controller is the single writer of `SnapshotContent.status.excludedRefs` = this node's own vetoes (read from the owning snapshot's `domainSpecificController.excludedRefs`) UNION every child content's aggregate (monotonic; a transient child NotFound never subtracts). The root Snapshot's own top-level veto drops are recorded by `parent_graph` into the root's `domainSpecificController.excludedRefs` so the aggregator reads "own direct exclusions" uniformly for root and domain nodes.
- **Add** top-level mirror: `mirrorSnapshotReadyFromBoundContent` (root) and `checkConsistencyAndSetReady`→`mirrorExcludedRefsFromContent` (domain CRs) copy the bound content's durable aggregate onto the namespaced `status.excludedRefs` (verbatim, guarded against clobbering on a transient content miss).
- Product decision (`w4a-events-decision`): field-only — `excludedRefs` is surfaced only via status fields; no d8-cli output and no Kubernetes Events in this wave.

### Verification (w4a-verify)

- Build: `api`, `pkg/snapshotsdk`, `images/{domain-controller,state-snapshotter-controller}` build cleanly; `go vet ./...` clean; integration package compiles under `-tags integration`.
- Unit tests: api (`resource_selector`), sdk (`exclude` — IsExcluded/PartitionExcluded/normalize/equal), core (`parent_graph` veto top-level drop; snapshotcontent aggregate own∪children + monotonic + round-trip), demo (VM planner veto partition) all pass; full controller module `go test ./...` green.
- `gofmt` clean on all touched files.

## Wave 4B — Recycle bin (partial: TTL default + domain StaticBind capture-skip)

- **Update** production recycle-bin retention default: `DefaultSnapshotRootOKTTL` = 30 days (720h) instead of the former `1m` DEBUG value. This is how long the durable cluster-scoped `SnapshotContent` tree survives after its namespaced `Snapshot` is deleted (the restore window). Rewrote `openapi/config-values.yaml` + `doc-ru-config-values.yaml` `snapshotRootOkTtl` descriptions accordingly; added `pkg/config` test (default 720h + env override/fallback).
- **Update** domain snapshot reconcilers (demo virtualdisk/virtualmachine) to skip capture when `IsStaticBind()`, mirroring the existing `IsImportMode()` guard: a StaticBind domain snapshot binds to a pre-provisioned surviving `SnapshotContent` and never runs live capture (no source lookup / MCR / children planning). Added a no-op reconcile test.
- **Deferred** (blocked on an API-contract decision — `spec.snapshotRef` mutability for restore: relaxed-immutability CEL vs. dedicated rebind subresource): generic domain `static_bind.go` core handling, tree-restore orchestration, d8-cli restore, and e2e. **Resolved in "Wave 4B — Recycle bin restore" below** (relaxed-CEL chosen); d8-cli restore + real-cluster e2e remain out of scope for that iteration.

## Wave 4C — Unified UID name scheme (in progress: api/names + demo)

- **Add** `api/names` — the single source of truth for the object-name scheme, depending on stdlib +
  apimachinery only (no controller-runtime/SDK) so both the core and the SDK can share one definition.
  `h8(s)=hex(sha256(s))[:8]`, `h16(s)=…[:16]`; generators `ChildSnapshotName(parentUID,sourceUID)` =
  `nss-snap-<h8>-<h16>`, `ContentName`/`ManifestCaptureRequestName`/`ManifestCheckpointName`/`ChunkName`/
  `OrphanVolumeSnapshotName`/`VolumeCaptureRequestName`/`ObjectKeeperName`. Names are opaque; connectivity
  stays on refs/ownerRefs. Unit tests cover hash widths, determinism, DNS-1123, and child-name uniqueness
  (different source, same source under two parents, per-PVC orphan MCR).
- **Add** SDK re-export `snapshotsdk.ChildSnapshotName` so domain controllers name sub-children with the
  same scheme via one definition.
- **Refactor** demo VM planner: removed `demoVirtualMachineDiskSnapshotName` (name-based `demovmdisk-…`);
  child disk snapshots are now named `snapshotsdk.ChildSnapshotName(vmSnapshotUID, diskUID)`. Updated
  `source_ref_test.go` accordingly.
- **Refactor** (w4c-core) core generator unification — every core object-name generator now delegates to
  `api/names` and keys purely on cluster-local UIDs:
  - SDK: `manifest.RequestName(snapshotUID)` and `storagefoundation.VCRName(snapshotUID)` delegate to
    `names.ManifestCaptureRequestName` / `names.VolumeCaptureRequestName`; `capture.go` passes `obj.GetUID()`
    directly (dropped the intermediate GVK computation).
  - Content: `snapshot.GenerateSnapshotContentName`, `snapshotContentName`, `snapshotbinding.StableContentName`
    → `names.ContentName(uid)` (name arg kept for signature compat but ignored / UID-only).
  - VCR: `volumecapture.SnapshotContentVCRName` / `SnapshotOwnedVCRName` → `names.VolumeCaptureRequestName`.
  - ObjectKeeper: `common.RootObjectKeeperName(uid)`, `snapshot.GenerateObjectKeeperName(uid)`,
    `namespacemanifest.ManifestCaptureRequestObjectKeeperName(uid)` → `names.ObjectKeeperName(uid)`; the
    post-deletion retained-content lookup (`retainedRootContentForSnapshot`) was rewritten to list
    `ObjectKeeper`s and match `spec.followObjectRef` (the deleted Snapshot's UID is gone, so the name can no
    longer be derived).
  - Parent graph: `snapshotChildSnapshotName(parentUID, sourceUID)` → `names.ChildSnapshotName` (GVK dropped).
  - Orphan (Variant A) keyed by the **orphan VolumeSnapshot UID** per ADR: `orphanPVCVolumeSnapshotName` →
    `names.OrphanVolumeSnapshotName`; child-node content → `names.ContentName(vsUID)`; per-orphan MCR →
    `names.ManifestCaptureRequestName(vsUID)`. `vsUID` is threaded through `orphanVSBindingResult` and read
    from the live VS at binding time.
  - MCP chunks → `names.ChunkName(mcpUID, i)` at both creation sites (`checkpoint_controller`,
    `import_manifest_reconstruct`); chunk names are read back from `ManifestCheckpoint.status.chunks[].name`,
    never re-derived.
  - `PublishSnapshotContentChildrenRefs` is now append-only (monotonic union, dedup+sort), removing the
    `volNodePrefix` heuristic and the `ChildVolumeContentInfix` constant — child classification no longer
    depends on name prefixes.
- **Update** (w4c-core) tests: unit (`namespacemanifest`, `common` OK-name, `snapshotbinding`,
  `volume_capture` orphan helpers keyed by a deterministic test VS UID, `import_manifest_reconstruct`) and
  integration (`snapshot_recreate`, `snapshot_root_deletion` use `snapshot.GenerateObjectKeeperName(uid)`;
  `manifestcapture_execution_ownerref` UID-only OK name). All module unit tests pass; integration + e2e
  packages compile; `gofmt` clean.
- **Deferred**: CSD `priority→weight` / `dataBacked→requiresDataArtifact`, MCP `manifestCaptureRequestRef`
  drop, and d8-cli (`ExportedSnapshot` rename, clusterUUID, import-replay).

## Wave 4B — Recycle bin restore (StaticBind end-to-end)

- **Update** (w4b-cel-relax) relax `SnapshotContent.spec.snapshotRef` immutability: dropped the field-level
  `self == oldSelf` rule and moved two object-level `XValidation` rules onto the `SnapshotContent` root
  (so CEL sees both `self.spec` and `self.status`): `snapshotRef` may change only once
  `status.parentDeleted` is latched (recycle bin), `deletionPolicy` stays immutable always. Regenerated
  `crds/…_snapshotcontents.yaml`. Rewrote `test/integration/snapshotcontent_spec_immutability_test.go`
  (reject re-point while parent alive, reject deletionPolicy change, allow re-point after parentDeleted,
  still reject deletionPolicy after parentDeleted); envtest run green.
- **Add** (w4b-domain-staticbind-runtime) domain StaticBind in the generic binder: new
  `genericbinder/static_bind.go` (`snapshotIsStaticBind`, `reconcileGenericStaticBind`,
  `genericStaticBindRefMatches`) plus a branch in `Reconcile` beside the import branch. A StaticBind domain
  leaf validates the back-reference handshake, binds `status.boundSnapshotContentName`, and mirrors
  Ready + `excludedRefs` from the existing content — running NO capture. Confirmed the demo capture-skip
  guard (`IsStaticBind()`).
- **Add** (w4b-restore-orchestration) core tree orchestration in `snapshot/static_bind.go`
  (`reconcileStaticBindRestoreTree`): after the root binds, walk the durable
  `SnapshotContent.status.childrenSnapshotContentRefs` graph and idempotently re-create each domain
  `XxxxSnapshot` child as StaticBind (`spec.mode: StaticBind` + `spec.source.snapshotContentName`, ownerRef
  → root Snapshot, deterministic name `names.ChildSnapshotName(rootUID, childContentUID)`), re-point each
  child content's `spec.snapshotRef` onto the re-created CR (relaxed-CEL, gated on `parentDeleted`), and
  recurse. Also reconstructs the root Snapshot's `status.childrenSnapshotRefs` (the tree the restore
  resolver walks), since a StaticBind root runs no capture wave.
- **Add** (w4b-orphan-vs-repoint) orphan volume-node leaf restore (Variant A). Capture now stamps
  `leafContent.spec.snapshotRef.uid` with the live orphan VolumeSnapshot UID (`orphan_pvc_volume_snapshot.go`),
  pinning the leaf↔VS identity and giving restore a concrete uid to re-point. On restore, the durable leaf
  content is NOT re-created; instead the CSI VolumeSnapshot handle is re-created pre-provisioned to the
  surviving Retain `VolumeSnapshotContent` (`spec.source.volumeSnapshotContentName`, ownerRef → root), the
  leaf back-reference is re-pointed to the new handle uid, and the INV-ORPHAN4 handle
  (`VolumeSnapshot.status.boundSnapshotContentName` → leaf) is written. Updated the resolver handshake
  comment (`usecase/restore/resolver.go`) to reflect that capture now stamps the uid.
- **Add** (w4b-tests) unit tests: `genericbinder/static_bind_test.go` (`snapshotIsStaticBind`,
  `genericStaticBindRefMatches`) and `snapshot/static_bind_restore_test.go` (domain re-create + re-point +
  root-tree reconstruction + idempotency + recursion; orphan leaf VS re-create + re-point + INV-ORPHAN4
  handle with the durable leaf content surviving). All module unit tests pass; integration test binary
  compiles; `gofmt` clean.
- **Note** monotonic `parentDeleted` (latch false→true only) leaves `snapshotRef` re-pointable after a
  restore — accepted as break-glass (RBAC gates writers).
- **Deferred**: d8-cli restore (`restore from bin` + `bin ls`) and real-cluster e2e (orphan VS
  readyToUse/boundVolumeSnapshotContentName come from the CSI snapshot-controller, not envtest); deep
  resolver walks of nested domain subtrees rely on each domain CR's own `status.childrenSnapshotRefs`
  reconstruction, validated when the domain binder StaticBind path runs on-cluster.

## Wave 4C (tail) — CSD/MCP API cleanup

- **Rename** (w4c-tail) `CustomSnapshotDefinition.spec.priority` → `spec.weight` (ascending order, lower runs
  first; documented against FlowSchema.matchingPrecedence / NodeGroupConfiguration.weight, explicitly NOT a
  PriorityClass). Updated the API type, `csdregistry/pairs.go` (`EligibleResourceSnapshotMapping.Weight` +
  ascending sort), the `parent_graph.go` weight-layer readers, and `templates/domain-controller/demo-csd.yaml`.
- **Rename** (w4c-tail) `CustomSnapshotDefinition.spec.dataBacked` → `spec.requiresDataArtifact`. Threaded
  through the API type, `UnifiedGVKPair.RequiresDataArtifact`, `GVKRegistry.MarkRequiresDataArtifact` /
  `RequiresDataArtifact` (map `requiresDataArtifactBySnapshotKind`), and every producer/consumer
  (`csdregistry/pairs.go`, `unifiedbootstrap`, `unifiedruntime/syncer`, `snapshotgraphregistry/build`,
  `genericbinder` controller/import/domain_content, `cmd/main`) plus the demo CSD template.
- **Remove** (w4c-tail) `ManifestCheckpoint.spec.manifestCaptureRequestRef` (the originating request is
  short-lived and never resolved by ref). Provenance now = `spec.sourceNamespace` + the
  `state-snapshotter.deckhouse.io/source-request` label. Dropped the field from the API type,
  `checkpoint_controller` create, `ReconstructManifestCheckpoint` signature (`captureRef` param removed) and
  `import_upload`; `archive_handler` reads the source request name from the label. Updated the doc-ru CRD.
- **Update** (w4c-tail) tests: `api/v1alpha1/manifestcheckpoint_test.go`, manifestcapture ginkgo/chunk-retry,
  `usecase` reconstruct/per-cr/archive, `internal/api/archive_handler` tests, and integration + e2e
  (`manifestcapture_execution_ownerref`, `snapshot_root_lifecycle`, `snapshot_aggregated_manifests`,
  `snapshotcontent_mcp_degradation_wakeup`, `e2e_test`) to the label/sourceNamespace provenance model.
- **Codegen** (w4c-tail) regenerated `api/v1alpha1/zz_generated.deepcopy.go` and the CSD/MCP CRDs. Module
  unit tests pass; integration + e2e packages compile; `gofmt` clean; no stale
  `priority`/`dataBacked`/`manifestCaptureRequestRef` identifiers remain in code.
- **Deferred**: obsolete `docs/…/snapshot-tree-demo/02-csd.yaml` still uses the pre-flat
  `snapshotResourceMapping` schema (invalid regardless of this rename); left as-is (out of core scope).

## Wave 4 — cross-verify (real-cluster e2e fixes)

- **Bugfix** (w4-cross-verify) Domain capture `phase` was not monotonic. The demo VM/disk reconcilers call
  `MarkPlanned` on every reconcile before switching on `CoreCaptureOutcome`, so a leaf that already reached
  `Finished` was dragged back to `Planned`; each reconcile then emitted two status writes (`Planned` then
  `Finished`) and, because the domain watches its own object, the pair re-triggered the reconcile — a
  self-sustaining phase write storm. The churn starved the core binder's optimistic-lock Ready mirror (34+
  "object has been modified" conflicts), wedging the tree at `Ready=False/ContentMissing` while the bound
  `SnapshotContent` was `Ready=True`. Guarded `setPhase` with `phaseCanAdvance`: the forward chain
  `Planning<Planned<Finished` never regresses (MarkPlanned is a no-op once Finished); `Failed` stays
  orthogonal (settable from any phase, recovery preserved). Added `pkg/snapshotsdk/capture_phase_test.go`.
- **Bugfix** (w4-cross-verify) Generic binder wedged the snapshot (and its parent) at
  `Ready=False/ContentMissing`: after the manifest checkpoint is archived the binder deletes the domain MCR
  but never clears `domainSpecificController.manifestCaptureRequestName`, so `ensureSnapshotContentLinks`
  chased the now-absent MCR and returned `requeue=true` at Step 4 before the Step 5 Ready mirror. Now skip
  the MCR chase once `commonController.manifestCaptured` is latched.
- **Update** (w4-cross-verify) SDK `EnsureManifestCapture`/`EnsureVolumeCapture` probe the informer cache
  for the MCR/VCR before the uncached `refresh`, paying the authoritative uncached read only when the
  request is absent — keeps the domain's in-flight `RequeueAfter` poll off the API server. Added
  `pkg/snapshotsdk/capture_refresh_test.go`.
- **Bugfix** (w4-cross-verify) `e2e/Makefile` `clean-env` deleted `snapshots.storage.deckhouse.io`; pointed
  the cascade at the current `snapshots.state-snapshotter.deckhouse.io` group so leftover Snapshots are
  actually removed.
- **Update** (w4-cross-verify) dev-image Makefiles (`images/domain-controller`,
  `images/state-snapshotter-controller`) pass `GO_VERSION` (read from `go.mod`) into the `Dockerfile-dev`
  build so the builder toolchain matches the module's required Go.

## Wave 5 — Root as in-process namespace-domain on SDK (dogfooding)

- **Add** (w5-api) `Snapshot.status.snapshotSource` (`*SnapshotSourceObjectRef`) on the namespace-root
  Snapshot so the root publishes its capture provenance (kind=Namespace) the same way domain snapshots do.
  Refreshed the `status.captureState` carve-out comment (the root now also carries
  `domainSpecificController.manifestCaptureRequestName`/`phase`, written by the in-process namespace-domain)
  and dropped the stale "absent on the namespace root" note on `SnapshotSourceObjectRef`. Regenerated
  deepcopy + `crds/state-snapshotter.deckhouse.io_snapshots.yaml`. Additive; no behavior change (the writer
  lands with the deferred namespace-domain reconciler rewrite).
- **Verify** (w5-orphan-node / w5-registration / w5-rbac) No code change needed — confirmed: Variant A orphan
  `VolumeSnapshot` child leaves are already emitted into `status.childrenSnapshotRefs[]`
  (`reconcileOrphanPVCVolumeSnapshotChildLeaves`) and consumed via `IsVolumeSnapshotVisibilityLeaf`; the
  built-in `Snapshot↔SnapshotContent` pair resolves without a CSD (`DefaultGraphRegistryBuiltInPairs`) and
  orphan-VS child content bypasses CSD via `EnsureVolumeChildContent`; the `040-namespace-capture-rbac` hook
  keys only on `commonController.manifestCaptured` (unaffected by folding content creation / adding
  `domainSpecificController` on the root).
- **Rename** (w5-field-rename, ss side) Hard-renamed the `SnapshotContent` data-role status (no back-compat
  aliases): `status.dataRef`→`status.data`, `SnapshotDataBinding.Target`→`Source`, dropped the standalone
  `SnapshotDataBinding.TargetUID` (the volume identity is now `data.source.uid`), and `SnapshotContent.DataRefList()`
  →`DataList()`. Regenerated deepcopy + `crds/state-snapshotter.deckhouse.io_snapshotcontents.yaml`. Updated all
  consumers: volumecapture (`validate`/`request_cleanup`/`unstructured`/`subtree_covered_pvc`/`domain_owned_targets`),
  `snapshotcontent/datarefs_publish`, `genericbinder` (`domain_content`/`import`), `snapshot`
  (`orphan_pvc_volume_snapshot`/`static_bind`/`volume_capture`), `volumesnapshotimport/controller`, the unstructured
  reader `pkg/snapshot/utils.go` (`status.data`/`data.source.uid`), the demo domain-controller restore path, the e2e
  readers (`status.data.source.name`), and the envtest structural schema in `test/integration/setup_test.go`. Updated
  all unit/integration fixtures + assertions; removed the obsolete "rejects empty targetUID" CRD-validation spec.
  Non-isolated integration suite + all unit tests green; e2e module compiles. (The `isolated` `duplicate pvcUID` spec
  times out on a pre-existing envtest limitation — no `VolumeSnapshotContent` CRD — proven identical on the base
  commit `5308a73`; unrelated to this rename.)
- **Update** (w5-status-source-descriptor, ss domain side) Made the namespaced data-leaf status self-sufficient
  for d8: replaced the flat top-level mirrors `status.storageClassName/size/volumeMode` on
  `DemoVirtualDiskSnapshotStatus` with a single self-contained top-level `status.data`
  (`*storagev1alpha1.SnapshotDataBinding`: source+artifact+volumeMode/fsType/accessModes/storageClassName/size).
  Rewrote the core mirror `genericbinder/domain_content.go` `mirrorLeafVolumeMetadataFromContent`→
  `mirrorLeafDataFromContent` + `mirrorVolumeMetadataToLeaf`→`mirrorDataToLeaf` (+ `snapshotDataBindingToMap`) to
  mirror the WHOLE `SnapshotContent.status.data` block verbatim (import still overrides storageClassName from
  `DataImport.spec.storageClassName`); updated both callers (capture path `controller.go`, import path `import.go`).
  Regenerated deepcopy + `crds/demo.state-snapshotter.deckhouse.io_demovirtualdisksnapshots.yaml`. Unit + non-isolated
  integration green; gofmt clean. Deferred: the extended-VolumeSnapshot fork `status.data` (storage-foundation
  patch 003 + VS CRD + `volumesnapshotimport` writer) — the fork patch applies to the upstream external-snapshotter
  tree not vendored here and cannot be compile-validated locally; kept the flat VS mirror with a `TODO(wave5)` marker.
- **Rename** (w5-field-rename, ss consumer side of the storage-foundation VCR/DataImport rename) Moved the
  cross-repo unstructured readers in lockstep with storage-foundation `20c48b5`: VCR `status.dataRef`→`status.data`
  (artifact-only) and DataImport `status.dataArtifactRef`→`status.data.artifact`. `ParseVolumeCaptureDataRefs`
  (`volumecapture/unstructured.go`) now reads the durable artifact from `status.data.artifact` and backfills the
  captured PVC identity from the immutable `spec.target` (VCR status no longer duplicates the target), so
  `ValidateDataRefsForPublish`/`SnapshotDataBindingsFromVCRStatus` and all callers are unchanged. Updated the
  DataImport artifact readers in `genericbinder/import.go` (`buildImportDataBinding`) and
  `volumesnapshotimport/controller.go` (`resolveDataImportArtifact`), the e2e DataImport readers
  (`diagnostics`/`backup_restore`), and all VCR/DataImport unit fixtures. All unit tests + non-isolated integration
  suite green; e2e compiles.
- **Add** (w5-tests, ss domain status.data mirror) Unit coverage for the wave5 self-contained data-leaf mirror
  in `genericbinder/domain_content_test.go`: `TestMirrorLeafDataFromContent_WritesTopLevelStatusData` (mirrors the
  whole `SnapshotContent.status.data` block onto the leaf `status.data` — source.uid/artifact/storageClassName/size —
  and asserts the removed flat `status.storageClassName/volumeMode/size` mirrors are NOT written),
  `TestMirrorLeafDataFromContent_ScOverride` (import path: `DataImport.spec.storageClassName` override lands in
  `status.data.storageClassName`), and `TestSnapshotDataBindingToMap` (JSON-typed map shape: source/artifact always
  present, `accessModes` as `[]interface{}`, empty optionals omitted). All green.
- **Note** (w5-content-creation + w5-namespace-domain-sdk) DEFERRED as one unit (hottest path, unsafe unattended):
  the binder's watch set excludes the built-in root `Snapshot` (`cmd/main.go` `FilterGenericSnapshotGVKPairs`), so
  folding root `SnapshotContent` creation into the binder cannot be separated from the full `snapshot/` → in-process
  SDK namespace-domain reconciler rewrite (two controllers would contend over `boundSnapshotContentName` / capture
  ordering). Left the working implementation intact; do attended behind the integration gate. Rationale + wiring
  facts in `.cursor/plans/wave5_notes.md`.
- **Note** (w5-cli + w5-d8-user-rbac, d8-cli) DEFERRED: the necessary local-type realignment
  (`SnapshotContentStatus` `dataRef`→`data`, `SnapshotDataBinding` `target`→`source` + drop `targetUID`) is green in
  d8 isolation today (self-consistent local copy; break only at runtime vs a not-yet-deployed wave5 cluster), and the
  namespaced BuildTree rewrite (drop cluster-scoped `SnapshotContent` reads; resolve from namespaced
  `status.snapshotSource`+`status.data`) depends on producers that are part of the deferred namespace-domain work
  (root `snapshotSource` writer, extended-VS `status.data`). Do d8 after those land. Details in `wave5_notes.md`.
- **Update** (w5-status-source-descriptor, extended-VS fork — Surface B, un-deferred) Reshaped the forked extended
  `VolumeSnapshot` `status` from the flat `storageClassName`/`size`/`volumeMode` mirror into a single self-contained
  top-level `status.data` (`VolumeSnapshotDataBinding`: source+artifact+volumeMode/fsType/accessModes/storageClassName/
  size), byte-identical to the domain data-leaf `status.data`, so d8 resolves the imported leaf's captured-volume
  descriptor from the namespaced VolumeSnapshot alone. ss side (compile-validated): rewrote `volumesnapshotimport`
  `mirrorVolumeMetadataFromDataImport`→`mirrorDataToImportVolumeSnapshot`, now mirroring the same enriched
  `SnapshotDataBinding` already published to the backing SnapshotContent (`enriched[0]`, SC overridden from
  `DataImport.spec.storageClassName`), through a new shared serializer
  `snapshotcontent.SnapshotDataBindingToUnstructuredMap` — extracted from genericbinder's private
  `snapshotDataBindingToMap` so both the domain-leaf and import-VS mirrors share one wire-shape source of truth
  (`TestSnapshotDataBindingToMap` retargeted). storage-foundation side (blind — the fork patch applies to the
  upstream external-snapshotter tree, not vendored here): reshaped `003-volumesnapshot-dataimport-fork.patch` (Go
  types + hand-written deepcopy, both `./client` and `vendor` copies) and the hand-maintained VS CRD
  (`snapshot.storage.k8s.io_volumesnapshots.yaml` + doc-ru, both `v1`/`v1beta1`) and patch README. ss build/vet/unit
  green; patch length-consistency verified via `git apply --numstat`.

## Wave 6 — DataImport / VolumeRestoreRequest honest spec (mode + Template/Ref)

Spec redesign of the two service resources onto the suffix convention: `...Template` = an object we
**create** (does not exist yet), `...Ref` = a reference to an **existing** object. The overloaded
`targetRef` (which conflated create vs. reference) is removed from both `DataImport` and `VRR`.
`DataExport.spec.targetRef` is untouched (it is an honest Ref to an existing source volume).

- **Refactor** (w6-vrr-api, storage-foundation) `VolumeRestoreRequest`: dropped `spec.targetRef` + root
  `storageClassName`/`volumeMode`/`accessModes` in favor of `spec.pvcTemplate` (PVC name in
  `pvcTemplate.metadata.name`); `spec.fsType` stays at the root (restore execution parameter read by the
  external-provisioner, not a PVC field); `status.targetRef`→`status.pvcRef` (with namespace + uid). Added
  `uid` to the shared `ObjectReference`. CRD + doc-ru + CEL + deepcopy regenerated with controller-gen
  v0.18.0 (the repo's actual version; v0.14.0 corrupted inline enums into `$ref`). VRR controller moved to
  `pvcTemplate`/`pvcRef`. (committed `b522440`)
- **Refactor** (w6-ss-reverse-lookup) `common/dataimport_lookup.go` `FindDataImportForLeaf` now matches a
  `DataImport` to a snapshot leaf by `spec.snapshotRef` (`apiVersion`→group, `kind`, `name`) instead of the
  old `spec.targetRef`; version-skew diagnostics reworded to `spec.snapshotRef`. Tests build a
  `ProduceArtifact` DataImport (`mode`+`snapshotRef`); a legacy-`targetRef` fixture is now a negative case.
- **Refactor** (w6-vrr-ss) `domain-controller` `demo/virtualdisk_restore.go` `buildDemoDiskVRR` now emits
  `sourceRef` + `pvcTemplate` (name + spec storageClassName/volumeMode/accessModes), `fsType` at root.
- **Bugfix** (w6-vrr-ss, storage-foundation — found by w6-verify grep) `data-manager-controller`
  `data-export/snapshot_resolver.go` `ensureVolumeRestoreRequest` was still building the old
  `spec.targetRef` VRR; moved to `sourceRef` + `pvcTemplate` (fsType at root). Test asserts
  `pvcTemplate.metadata.name`/`spec.volumeMode` and absence of `spec.targetRef`.
- **Update** (w6-e2e) e2e fixtures: `createDataImport(apiVersion)` → `mode: ProduceArtifact` +
  `scratchVolumeTemplate` + `snapshotRef` (apiVersion corrected to `storage-foundation.deckhouse.io/v1alpha1`);
  `createVolumeRestoreRequest` → `pvcTemplate` (apiVersion corrected from the wrong `state-snapshotter…` to
  `storage-foundation…`); `diagnostics_test.go` reads `spec.snapshotRef`; demo YAML
  `04-volumerestorerequest.template.yaml` → `sourceRef`+`pvcTemplate`.
- **Update** (w6-docs, cross-repo) d8-cli `internal/data/dataimport/README.md` (`mode: PopulateVolume` +
  `pvcTemplate`) and `docs/CHEATSHEET_E2E.md` (`mode: ProduceArtifact` + `snapshotRef` +
  `scratchVolumeTemplate`); demo runbook + notes updated (`dataRefs[]`→ singular `status.data`, working
  jsonpath `.status.data.artifact.name`, VRR `sourceRef`+`pvcTemplate`). Historical design/spec docs left
  intact.
- **Update** (w6-d8-snapshot, d8-cli) `snapimport/volume.go` `EnsureDataImport` → `mode: ProduceArtifact` +
  `snapshotRef` + `scratchVolumeTemplate`; `leafTargetRef`→`leafSnapshotRef` (returns apiVersion+kind);
  `dataImportCompleted` reads `status.data.artifact` (was `status.dataArtifactRef`).
- **Update** (w6-d8-standalone, d8-cli) typed `DataImport` API + `util` → top-level `Mode`
  (`ModePopulateVolume`) + `PvcTemplate` (removed `TargetRef`/`DataImportTargetRefSpec`).
- **Note** (w6-d8-vrr) N/A: d8-cli neither creates nor reads `VolumeRestoreRequest`; VRR is produced only by
  the domain-controller (state-snapshotter) and the data-export controller (storage-foundation).
- **Update** (w6-vrr executor, storage-foundation, blind per user request) Rewrote the csi-external-provisioner
  `002-vrr-executor.patch` to the wave6 VRR schema: `vrr_handler.go` reads `spec.pvcTemplate`
  (name/storageClassName/volumeMode/accessModes) + root `fsType`, target namespace = the VRR's own namespace,
  with SF→corev1 mirror-type converters (`convertAccessModes`/`vrrVolumeMode`/`vrrStorageClassName`/
  `vrrTargetPVCName`/`vrrTargetNamespace`); test rewritten to `pvcTemplate`. New-file hunk headers recomputed
  (821→862, 1073→1084).
- **Update** (w6-vrr executor verify, storage-foundation) wave6 api pushed (commit `2c52550`); re-pinned
  `002-vrr-executor.patch` `b97b1e1` → `v0.0.0-20260706134706-2c525506f13c` (+`go.sum` hashes) and bumped the
  `go` directive `1.25.10` → `1.26.4` (`api/go.mod` requires 1.26.4). Verified end-to-end: `001`+`002` apply
  cleanly on a pristine `v6.2.0` tag, `CGO_ENABLED=0 go build ./cmd/csi-provisioner` builds (~98 MB) and
  `go test ./pkg/controller/` (all 25 VRR tests) passes against the pinned wave6 api. Patch README + `oss.yaml`
  updated: BLOCKER removed, patch-on-tag model documented. Remaining follow-up is operational only — swap the
  pseudo-version for a published `api/vX.Y.Z` tag once it exists.
- **Note** (w6-di-controller) `volumeRef`+`force` overwrite path is a fail-closed stub (net-new populator
  logic, follow-up); `virtualDiskTemplate` deferred per ADR.

### Verification (w6-verify)

- Grep `targetRef`/`TargetRef` in active DataImport/VRR code across storage-foundation + state-snapshotter +
  d8-cli: the only remaining matches are the intentional `DataExport.spec.targetRef` (unchanged this wave)
  and the `SnapshotDataArtifactRef` SnapshotContent type (unrelated). No stale `status.dataArtifactRef`.
- Build + unit tests green: storage-foundation `data-manager-controller` (`data-export` + `data-import`);
  d8-cli `snapimport` + `data/dataimport` (go vet + test, gpgme sidestepped via targeted packages).
- gofmt clean on all changed Go files.

## Wave 7 — Ready/conditions model: capture-controller repartition + late Planned

- **Bugfix** (w7-creator, green-restore) The generic binder never watched the built-in root `Snapshot` at
  pod startup, so root `SnapshotContent` was never created/bound and the whole root capture path hung
  pre-bind (envtest RED: N2a root-lifecycle / frozen-plan / MCR-GC / aggregated-manifests). Root cause: since
  wave5 the root `Snapshot` is a domain-capture kind owned by the binder, but `FilterGenericSnapshotGVKPairs`
  still strips every dedicated kind (root included) from the startup watch set, and the only compensating
  registration — `unifiedruntime.Syncer.Sync` — runs on CSD reconciles, never at boot (documented deferral,
  this WORKLOG "w5-content-creation" note). Fix: added `unifiedbootstrap.StartupDomainCaptureRootPair` and,
  in both `cmd/main.go` and `test/integration/setup_test.go`, register the built-in root pair on the binder
  at startup (`MarkDomainCaptureKind` + `AddWatchForPair`, idempotent w.r.t. a later `Sync`; unlike demo
  kinds the built-in root needs no RBAC gating). The 4 N2a specs pass (305s→~26s, no more timeout-blocked
  `Eventually`s); unit + build + vet green. NOTE: the full `w7-creator` single-writer contract (creator
  writes `Snapshot.Ready` only pre-bind/only `False` via `ContentBindingPending`; never after bind) lands
  together with `w7-main-split`/`w7-final-wave-1`, where the post-bind mirror (`checkConsistencyAndSetReady`)
  moves to the `main` reconciler and `ContentBindingPending` is introduced — the binder today writes Ready
  only post-bind, so no pre-bind violation exists to fix in isolation.
- **Note** (w7-creator, remaining pre-existing root-path reds) Full `!isolated` integration pass is **52/55**
  after the green-restore; the 3 still-red specs are **pre-existing** (verified RED at the wave5 baseline via
  `git stash` — all three gate on `status.boundSnapshotContentName`, which was never set while root was
  unwatched — so they are NOT regressions) and each is owned by a later wave7 task:
  (1) `snapshot_content_ready_propagation_test.go` "mirrors Ready=True when only SnapshotContent status
  changes" — asserts a PURE mirror; the pre-split binder re-derives Ready every reconcile
  (`checkConsistencyAndSetReady` → "Deriving Snapshot Ready" ManifestCapturePending), clobbering the test's
  manual `content.Ready=True`. Structurally requires **w7-main-split/w7-final-wave-1** (pure mirror). (2)
  `snapshot_root_lifecycle_test.go` "reaches Ready with empty MCP when there are no allowlisted namespaced
  resources" — empty-namespace manifest leg never publishes a checkpoint (**w7-reorder-planning / capture
  completion**). (3) `snapshot_root_deletion_test.go` "Delete: root finalizer clears only after
  SnapshotContent is gone" — test pre-creates the content; the binder's `Create` doesn't handle
  `AlreadyExists`→adopt, so it never binds (**w7-creator adopt path / w7-main-split**). Full suite green is the
  `w7-verify` gate.
- **Add** (w7-main-split, content-controller Ready mirror) The `SnapshotContent` reconciler (reconcile key
  = `SnapshotContent`) now mirrors the just-computed `content.Ready` onto the owning `Snapshot.Ready` in the
  SAME reconcile pass (`internal/controllers/snapshotcontent/ready_mirror.go`
  `mirrorReadyToOwnerSnapshot`), called from `Reconcile` right after `reconcileCommonSnapshotContentStatus`.
  Owner is resolved from `content.spec.snapshotRef` (apiVersion/kind/namespace/name); the mirror is gated on
  the monotonic creator->main writer switch (`owner.status.boundSnapshotContentName == content name`), skips
  ownerless/bucket content and non-bind owner kinds (e.g. a `VolumeSnapshot` leaf handle), bubbles a domain
  `phase=Failed` into a terminal Ready=False, and patches under an optimistic-lock merge (gen-stamped). This
  collapses the former cross-controller hop that let the binder re-derive a stale Ready (INV-FAIL-PROP).
  Staged per plan: the binder's `checkConsistencyAndSetReady` mirror stays as a converging fallback (both
  writers derive the same value under a changed-gate) and the binder keeps the `excludedRefs` /
  `subtreeManifestsPersisted` mirrors; **w7-final-wave-1** removes the binder's Ready mirror + content->snapshot
  watch and relocates the two remaining mirrors here (single post-bind writer). Verify: build + gofmt + unit
  (snapshotcontent, genericbinder) green; full `!isolated` integration shows the same 3 pre-existing reds and
  **no new failing spec** — E5 (`snapshot_graph_integration`) and N1 (`snapshot_n1_boundary`) are pre-existing
  timing-flaky specs (both also fail on the stashed baseline under load, E5 at a different assertion), not
  regressions.
- **Add** (w7-adr-conditions) Normative "Conditions & Reasons" catalog added to the ADR
  (`arch/.../2026-06-29-unified-snapshots-overview.md`, under "Модель статусов"): condition types
  (`Ready` user-facing + core-internal `ManifestsReady`/`VolumeReady`/`ChildrenReady`), the exact
  terminal set (`TerminalReadyReasons`) vs non-terminal reasons, the single-reason priority order, the
  wave7 `Snapshot.Ready` writer switch (pre-bind creator `False`/`ContentBindingPending` → post-bind main
  mirror on monotonic `boundSnapshotContentName`), and the wave7 catalog deltas (new fail-closed
  `ChildrenLinkPending`; `ResidualVolumeCapturePending` marked REMOVED). Strings kept verbatim to
  `api/storage/v1alpha1/conditions.go` + `pkg/snapshot/conditions.go`. (ADR committed by user.)
- **Refactor** (w7-d8-import-namespaced) [d8-cli repo, commit bf62a8d] Import readiness waits on the
  node's OWN namespaced object instead of the cluster-scoped `SnapshotContent`: core `Snapshot` root and
  domain data leaf poll their own `Ready=True` (a faithful all-legs mirror post-bind, wave7 INV-FAIL-PROP);
  a CSI `VolumeSnapshot` leaf polls `status.readyToUse` (no `Ready` condition). Removed
  `waitSnapshotContentReady` + `snapshotContentGVR` + per-leg condition constants; the CLI no longer needs
  cluster-scoped `snapshotcontents` access. Package + repo build/vet/test green.
- **Add** (w7-content-free-coverage, stage 1 of the capture-core reorder) Root-residual/orphan PVC coverage
  is now computed CONTENT-FREE of the ROOT: new
  `usecase/volumecapture.CollectSubtreeCoveredPVCUIDsFromSnapshot` walks the Snapshot child graph
  (`status.childrenSnapshotRefs`, populated at planning time before the root content bind), reads each
  descendant node's OWN already-bound `SnapshotContent` (`status.boundSnapshotContentName`), and reuses
  `coveredPVCUIDsForContent` so the covered-UID set is identical to the content-tree walk
  (`CollectSubtreeCoveredPVCUIDs`). Skips CSI `VolumeSnapshot` visibility leaves (residual wave's own
  output), fails closed on unreadable child/bound-content and on duplicate covered UID
  (`ErrDuplicateCoveredPVCUID`). `listResidualRootOwnedPVCTargets` + `IsResidualRootPVCCaptureScope`
  (`domain_owned_targets.go`) switched to this Snapshot-derived path (bound-content arg retained, unused).
  This breaks the "late Planned" circular dependency (residual/orphan discovery no longer needs the root's
  bound content). Runtime-safe: the same GVK Gets already run in the preceding
  `allDeclaredDomainChildSnapshotsReady` gate. Unit tests (new walker + updated residual test) + full module
  build green; integration deferred to the end-of-wave `w7-verify` loop.
- **Decision** (w7-reorder-planning scoping) Resolved an internal ADR contradiction on the root-MCR
  manifest-exclude source for "late Planned". ADR §"Late Planned" (str. 267-271/290-296) literally reads
  exclude from descendant `ManifestCaptureRequest.spec.targets[]` at `Planned`, but MCRs are EPHEMERAL (the
  binder deletes the MCR after `manifestCaptured=true`), so under bottom-up `Planned` a child MCR can be
  captured+deleted before the root builds its MCR → exclude under-counts → 409/double-capture. Per user
  ("решение на новый эндпоинт на api сервисе на snapshotcontent") the DURABLE source is the existing
  `snapshotcontents/<name>/subtree-manifest-identities` subresource (`usecase.BuildSubtreeManifestIdentities`,
  fail-closed 409, gated by `subtreeManifestsPersisted`) — NOT ephemeral MCR targets. Chosen scope (variant
  A, matches ADR §287-288 + the removal todos): late-Planned reorders ONLY the VOLUME axis (orphan/residual
  PVC wave + content-free PVC coverage move BEFORE `MarkPlanned`, gate flips from "children Ready" to
  "children `phase>=Planned`" via `allDirectDomainChildrenAtLeastPlanned`); the MANIFEST axis
  (`BuildRootNamespaceManifestCaptureTargets` + `subtreeManifestsPersisted` wave-barrier / subtree-identities
  endpoint) is left intact. Prereq refactors: make `BuildRootNamespaceManifestCaptureTargets` and the orphan
  VS-creation leg ROOT-content-free (discover descendants via `status.childrenSnapshotRefs`, not the root
  content tree) so the root MCR + orphan VS can be created before the binder binds the root content; the
  orphan child-content LINKING (`reconcileOrphanPVCVolumeSnapshotPublish`) stays POST-bind.
- **Refactor** (w7-reorder-run + w7-rcf-orphan) "Late Planned" VOLUME-axis reorder in
  `reconcileNamespaceCapture` (`namespace_capture_run.go`): the orphan/residual PVC wave (CSI VolumeSnapshot
  creation + `childrenSnapshotRefs` leaf publish) now runs CONTENT-FREE and BEFORE `MarkPlanned`, so the full
  child set (domain children + orphan leaves) is enumerated when the binder creates+freezes the root content
  — no orphan is missed from the frozen `childrenSnapshotContentRefs`. New
  `ensureOrphanVolumeSnapshotsPrePlanned` gates on all domain children Ready (guarantees complete
  subtree-covered PVC coverage under the current bound-content coverage reader), computes residual targets via
  `ListOwnedPVCTargetsForLogicalContent(..., nil)`, and creates VS with no bound root content;
  `ensureOrphanPVCVolumeSnapshots` routes terminal orphan-class failures via `failOrphanCaptureTerminal`
  (content!=nil → `failCapture`; pre-bind content==nil → Snapshot's own `Ready`). The orphan child-content
  LINKING (`reconcileVolumeCapturePublish`) and the MANIFEST leg stay POST-bind (manifest axis unchanged —
  wave-barrier). Dead `reconcileCaptureN2a` (capture.go) untouched (not on the live path). New unit tests
  cover the content-free create + the children-Ready gate; full module build+unit green.
- **Bugfix** (w7-reorder-run) `ListOwnedPVCTargetsForLogicalContent` dropped its early
  `if content == nil { return nil, nil }` guard, which silently returned zero targets and would have made the
  content-free pre-Planned wave a no-op; the residual root path is content-free by design and the domain path
  is already nil-safe. Updated the stale `TestListOwnedPVCTargets_duplicateSubtreePVCFailsClosed` (pre-existing
  failure since the Stage-1 content-free coverage flip: it built the duplicate on the ROOT content tree, which
  the residual path no longer walks) to build the duplicate on the Snapshot child graph and pass `nil` content.
- **Update** (fix-adr) Resolved the ADR §"Late Planned" self-contradiction on the manifest-exclude source
  (`arch/.../2026-06-29-unified-snapshots-overview.md`): content-free planning is now scoped to the VOLUME
  axis (orphan enumeration from the Snapshot child graph); the MANIFEST-exclude set is explicitly a
  wave-barrier fed by the durable `snapshotcontents/<name>/subtree-manifest-identities` subresource gated by
  `subtreeManifestsPersisted` (NOT ephemeral descendant `MCR.spec.targets[]`), so the root MCR is built
  post-bind. Redefined PIT = moment of `Planned` = FREEZE of `childrenSnapshotContentRefs` (decoupled from
  root-MCR creation); fixed the sequence-diagram note (orphan VS + `childrenSnapshotRefs` before Planned,
  per-PVC MCR/linking post-bind). Added an implementation-interim note: the volume wave currently gates on
  children `Ready` (bound-content coverage) rather than the target `phase>=Planned` (VCR-based coverage).
- **Remove** (w7-remove-residual + w7-childrenready-recompute) Deleted the `residualVolumeCapture` latch
  mechanism and folded the orphan/residual-PVC wave into `ChildrenReady`. Gone: API type
  `ResidualVolumeCaptureStatus` + `SnapshotContentStatus.residualVolumeCapture` field + phase consts (api +
  deepcopy + `crds/...snapshotcontents.yaml`), the writer `MarkResidualVolumeCaptureComplete` (status_publish.go)
  and all 4 callers (orphan_pvc_volume_snapshot.go ×2, static_bind.go, import.go), the aggregator gate
  `computeResidualSweepGate` + `residualSweep*` plan legs + the lowest-priority Ready switch case
  (snapshotcontent/controller.go), and `ReasonResidualVolumeCapturePending` (api/storage + pkg/snapshot
  aliases). Replacement: `validateCommonContentChildren` now runs a fail-closed declared-vs-linked check for
  orphan children — `unlinkedOrphanChildContents` reads the owner's `status.childrenSnapshotRefs`, keeps the
  CSI VolumeSnapshot visibility leaves, derives each orphan child content name from the live VS UID
  (`orphanChildContentNameFromVSLeaf`, skips a fresh-NotFound VS = reconstructed import/restore leaf), and
  holds `ChildrenReady=False/ChildrenLinkPending` until each is linked into `childrenSnapshotContentRefs`.
  Monotonic upgrade-guard (skips once this content's Ready is already True) preserves the removed gate's
  "blocks only the FIRST Ready=True" contract. New reason `ReasonChildrenLinkPending` (api/storage canonical +
  pkg/snapshot alias, non-terminal). Because the archive one-way latch (`subtreeManifestsPersisted` /
  `declaredNonLeafChildContentNames`, permanent-duplicate-capture risk) was left untouched, the worst-case of
  a wrong orphan-set is Ready stuck False (liveness, detectable), never a silent duplicate capture. Tests:
  deleted residual_gate_aggregation_test.go / residual_volume_capture_test.go /
  snapshot/residual_gate_stamp_test.go; retargeted the reconcile-count MergeFrom clobber-safety test onto the
  `status.parentDeleted` sibling field; repointed the RBAC hook predicate test to `ReasonChildrenLinkPending`;
  added orphan_link_gate_test.go (unlinked→ChildrenLinkPending, linked→Ready, leaf/non-root/foreign-apiVersion
  ungated, no-VS-leaves/missing-VS skipped, upgrade-guard, pending-child priority). Build + unit green across
  api / controller / hooks modules. NOTE: the integration + e2e residual tests
  (test/integration/*, e2e/tests/ready_flap_test.go — build-tagged / separate module) still reference the
  removed symbols and are deferred to the w7-verify integration loop.
- **Add** (w7-barrier2) Revived the barrier-2 Ready-finalization gate (ADR §6.2: the core finalizes a
  domain snapshot's user-facing `Ready=True` ONLY after `captureState.domainSpecificController.phase=Finished`).
  Folded a fail-closed `phase=Finished` check into BOTH post-bind Ready writers so they agree during the
  staged creator→main split: the binder `checkConsistencyAndSetReady` (genericbinder/controller.go, reviving
  the previously dead `domainCaptureFinished`) and the content controller `mirrorReadyToOwnerSnapshot`
  (snapshotcontent/ready_mirror.go, new `ownerDomainCapturePhase` helper; `ownerDomainCaptureFailed` routed
  through it). While a domain owner is at `phase in {Planning,Planned}` and the content is already Ready=True,
  the aggregate is held `Ready=False/ChildrenPending` (non-terminal, "как childrenPending" per ADR); the
  content reconcile re-runs on the owner-status watch when the phase advances. Precedence: `phase=Failed` is
  still bubbled ahead of the gate (the gate only engages when the mirrored status would be True); a non-domain
  owner (import/static-bind/leaf handle — no `phase` field) mirrors verbatim, unaffected. No deadlock:
  `phase=Finished` is driven by the latch-based `CoreCaptureOutcome` (ConfirmConsistent), independent of
  `Ready`, so Ready and Finished converge from the same capture latches. Tests: added
  genericbinder/barrier2_test.go and snapshotcontent/ready_mirror_barrier2_test.go (Planned holds /
  Finished finalizes / no-phase verbatim / Failed bubbles first). Build + gofmt + unit green (controller
  module). w7-final-wave-1 (collapse to the single content-controller writer — remove the binder Ready mirror
  + content→snapshot watch, relocate the excludedRefs/subtreeManifestsPersisted mirrors) stays deferred to the
  w7-verify integration loop, as it removes the converging fallback and a watch (liveness-sensitive).
- **Update** (w7-verify, compile-fix) Repaired the deferred residual-referencing tests so the integration
  package (`-tags integration`) and the separate e2e module both compile again. Dropped the now-removed
  `c.Status.ResidualVolumeCapture = ...Complete` seed from the three envtest specs
  (snapshotcontent_ready_contract_test.go, snapshotcontent_mcp_degradation_wakeup_test.go ×2,
  genericbinder_parent_degradation_content_driven_test.go) — the orphan-link ChildrenReady gate is vacuously
  open there because the owning Snapshot is never created (unlinkedOrphanChildContents fail-opens on a
  NotFound owner), so only the surviving subtreeManifestsPersisted latch still needs seeding. Rewrote the
  e2e ready_flap diagnostic (contentDiagExtract) to print ChildrenReady status/reason (surfacing the
  ChildrenLinkPending gate) instead of the removed status.residualVolumeCapture.phase. `go vet` green for
  both modules; envtest/e2e RUN validation still pending in the w7-verify loop.
- **Refactor** (w7-final-wave-1) Collapsed the steady-state Snapshot.Ready mirror to a SINGLE post-bind
  writer — the SnapshotContentController (`snapshotcontent/ready_mirror.go: mirrorReadyToOwnerSnapshot`),
  which already mirrors content.Ready + bubbles phase=Failed + applies the barrier-2 (phase=Finished) gate in
  the same pass that computes content.Ready (no cross-controller staleness, INV-FAIL-PROP; ADR wave7 note
  "зеркало считает main тем же проходом"). Removed the binder's happy-path Ready re-derivation from
  `genericbinder/controller.go: checkConsistencyAndSetReady` (and the now-dead `domainCaptureFinished` /
  `domainCaptureFailed` helpers in domain_content.go — barrier-2/Failed now live only in the content
  controller). The binder RETAINS: the pre-bind and content-missing/deleting degradation Ready co-write (a
  deleted content produces no content reconcile to mirror from — the binder is woken for it by
  `mapBoundContentToSnapshots`), plus the excludedRefs / subtreeManifestsPersisted side-channel mirrors
  (not Ready, triggered by the same watch). Decision: the bound-content watch and the two side-channel
  mirrors are NOT removed/relocated (the original w7-final-wave-1 sketch) because the watch is still required
  for the content-deletion degradation path; relocating the side channels would add regression risk on the
  manifest-exclude pre-gate for no functional gain. Tests: removed genericbinder/barrier2_test.go (binder no
  longer gates Ready); rewrote genericbinder/mirror_test.go to assert the binder co-writes ContentMissing
  when the bound content is gone   and does NOT overwrite Ready when the content is present. Build + gofmt +
  full controller-module unit green; integration module `go vet` green. Runtime validation deferred to the
  w7-verify integration loop.
- **Add** (w7-immutable-children-cel) Enforced the frozen/append-only `childrenSnapshotContentRefs` at the
  API level. Added a kubebuilder XValidation transition rule on
  `SnapshotContentStatus.ChildrenSnapshotContentRefs` (api/storage/v1alpha1/snapshotcontent_types.go) and
  regenerated the SnapshotContent CRD via controller-gen v0.18.0. Rule: `self.size() >= oldSelf.size()`
  (message: append-only, must not shrink). Chose the O(1) size-monotonic rule over a per-entry
  `oldSelf.all(x, self.exists(y, y.name == x.name))` no-removal check: the latter is O(n*m) over an
  unbounded list with an unbounded child name and would blow the apiserver CEL estimated-cost budget (CRD
  rejected at apply → whole envtest suite fails to start) unless the list and name were artificially capped;
  since BOTH writers are strictly append-only (PublishSnapshotContentChildrenRefs preserves every existing
  edge then adds missing ones; LinkChildVolumeContentRef appends; a failed child stays a node, E3), no-shrink
  is equivalent to no-removal here. Deliberately NOT marked `Required`: a volume leaf legitimately has no
  children (empty/omitted). Belt-and-suspenders for the code-level optimistic-locked append invariant. api +
  controller build/vet + api unit green; CEL runtime enforcement validated only against a real apiserver
  (envtest) in the w7-verify loop.
- **Refactor** (w7-verify, n5_pr7-csi-simulator) Rewrote the N5 PR-7 integration specs
  (test/integration/n5_pr7_two_pvc_integration_test.go) for the wave7 orphan-wave model, where a residual/loose
  namespace PVC is captured as its OWN standalone child volume node (own SnapshotContent + dataRef) instead of
  being appended to the root aggregator MCR — so the root MCR never carries a PVC manifest. Added a self-contained
  CSI simulator (test/integration/n5_pr7_csi_simulator.go): installs the cluster-scoped VolumeSnapshotClass +
  VolumeSnapshotContent CRDs with a ROOT-level `x-kubernetes-preserve-unknown-fields` (the shared BeforeSuite
  installs only VolumeSnapshot, and a spec/status-only preserve prunes the VolumeSnapshotClass top-level `driver`,
  breaking orphan class resolution); seeds the shared StorageClass(+volumesnapshotclass annotation)/VSClass and
  CSI-backed bound PV/PVC fixtures; and runs a fake external-snapshotter reactor (uncached direct client) that
  binds each orphan VolumeSnapshot the controller creates (creates a bound VSC, patches VS
  status.readyToUse+boundVSCName). Scoped to a dedicated `isolated` Serial+Ordered `go test` pass (the reactor's
  VolumeSnapshotContent CRD would perturb !isolated specs that rely on it being absent). Three specs pass green:
  residual CSI PVCs each become their own child volume node (root MCR carries no PVC); root MCR still captures a
  plain ConfigMap while excluding the residual CSI PVC; and the DuplicateCoveredPVCUID guard fires on two subtree
  contents claiming the same PVC UID — the latter needed a real ready VSC behind the colliding dataRef, because
  wave7 now fails a dataRef whose artifact VSC is missing (ArtifactMissing→ChildrenFailed) before the duplicate
  guard runs. Marked the pending-VCR-coverage spec Pending: reproducing an in-flight (dataRef-less) VCR on a
  subtree child at the integration level under wave7 is inherently racy (only a synthetic empty-spec namespace
  child can carry it, and its own volume-leg readiness races the fixture / collides on the ObjectKeeper lifecycle
  ownerRef); the mechanism (`pvcUIDsFromPendingVCR`) stays covered deterministically by the unit test
  `TestCollectSubtreeCoveredPVCUIDs_pendingVCRTargets`. Isolated pass green twice; removed obsolete `pr7CreatePVC`
  helper (replaced by `pr7CreateCSIPVC`) and stray debug logging.
- **Add** (w7-verify, e2e) Added the domain-VolumeCaptureRequest subtree-coverage assertion the integration
  pending-VCR spec could only cover as a unit test, now against the live demo domain kind. New spec in
  e2e/tests/volumedata_test.go ("excludes domain-VolumeCaptureRequest-covered PVCs from the root own-manifests")
  reuses the phase-3 vol-tree and asserts, at steady state, that every source PVC is excluded from the root's
  own manifest checkpoint (demo-pvc-disk/demo-pvc-standalone are domain-VCR-covered under their DemoVirtualDisk
  snapshots; demo-pvc is a root orphan child volume node), that the root carries no PersistentVolumeClaim
  manifest at all (Variant A), and — as a positive control — that the demo ConfigMap IS captured. Also added a
  best-effort background observer (startPendingVCRWindowObserver + pvcHasPublishedDataRef/vcrTargetsPVC helpers,
  new volumeCaptureRequestGVR in e2e_shared_test.go) started in the capture spec that records whether the run
  caught the transient window where a domain disk VolumeCaptureRequest targets demo-pvc-disk before its dataRef
  publishes; the result is logged (GinkgoWriter), never asserted, since a fast cluster may publish between polls.
  The transient mechanism (pvcUIDsFromPendingVCR) stays deterministically unit-covered; the new e2e proves the
  steady-state exclusion it enables. e2e module builds/vets green.

## Wave 8 — Content single-writer (domains content-free)

- **Design** (plan-only, no code) Added docs/content-single-writer-design.md: bring order to who writes
  SnapshotContent. Two rules — (1) domains, including the in-process namespace domain (SnapshotReconciler,
  internal/controllers/snapshot/), never write SnapshotContent; they publish only onto their own
  Snapshot.status (childrenSnapshotRefs/phase/data); (2) status.childrenSnapshotContentRefs has a single
  writer — the SnapshotContentController aggregator — which projects the owner's childrenSnapshotRefs into
  child edges (the aggregator already resolves both domain children via declaredNonLeafChildContentNames and
  orphan volume-node leaves via unlinkedOrphanChildContents, today only to fail-close). Removes the two
  append-only co-writers (genericbinder PublishSnapshotContentChildrenFromSnapshotRefs + snapshot
  LinkChildVolumeContentRef) and the optimistic-lock dance they needed. Staged migration: slice 1 child edges
  → aggregator; slice 2 MCP-name → binder; slice 3 data leg + orphan child-content creation → core (open
  question: which core controller creates orphan child-volume-node contents). Explicitly NOT the pre-Planned
  orphan-wave deadlock fix.
- **Design** (plan-only, no code) Extended docs/content-single-writer-design.md (§3.4 + slice 4) to make
  status.childrenSnapshotContentRefs immutable (frozen set), strengthening the current no-shrink CEL rule
  (self.size() >= oldSelf.size()) to set-once immutable. Precondition: the single writer must emit the
  COMPLETE frozen child set in one atomic write (Late Planned, phase>=Planned) — incremental append would be
  rejected by an immutable rule, so slice 4 lands AFTER the atomic-write end-state. Recommended CEL Option A
  "immutable once set": oldSelf.size() == 0 || self == oldSelf (O(n), within apiserver CEL cost budget;
  decoupled from content-creation timing); Option B strict self == oldSelf requires create-time population.
  Added INV-CONTENT-CHILDREN-2, envtest CEL acceptance (reject post-set shrink/append/reorder/replace, allow
  first set), and risks (ordering mandatory; audit for hidden field rewriters in teardown/degradation).
- **Design** (plan-only, no code) Added §3.5 "Write barrier" to docs/content-single-writer-design.md: the
  precondition for writing childrenSnapshotContentRefs is stronger than "children declared + phase>=Planned"
  — the write commits only when (1) childrenSnapshotRefs is complete/frozen (Late-Planned enumeration incl.
  orphan leaves) AND (2) every declared child has materialized content (domain: boundSnapshotContentName
  resolvable; orphan: child-volume-node content exists) — the exact pair the aggregator already computes for
  its fail-closed Ready gate. Decision (2026-07-07): incomplete set = parent stays pending (ChildrenLinkPending),
  never a partial write and never a timeout; a terminal child failure surfaces via the child's own Ready reason
  (ChildrenFailed).   Noted content names are deterministic (names.ContentName(uid)) so early write at Planned is
  possible but rejected (edges must never dangle, doubly so under immutable). Confirmed no cycle (child content
  needs parent content to EXIST, not the parent's childrenSnapshotContentRefs). Added INV-CONTENT-CHILDREN-3
  (edges never dangle) and a risk that immutable correctness depends on a genuine Late-Planned enumeration.
- **Design** (plan-only, no code) Added §8 "Related design" to docs/content-single-writer-design.md capturing
  the 2026-07-07 discussion: (8.1) proposed subtreePlanned latch (analog of subtreeManifestsPersisted for the
  planning phase; subtreePlanned(n) = planned(n) AND all descendants planned, computed via direct children;
  content-write gate uses only the OWN planned leg, wave completion uses the recursive latch; placement
  snapshot-native vs content-native left open); (8.2) relationship to subtreeManifestsPersisted (three ordered
  properties planned <= edge-linked <= persisted; subtreePlanned CANNOT replace the persisted latch because the
  manifest-exclude is persisted-based; but the frozen-set weakens the latch's fail-closed guard role -> possible
  follow-up to drop the field, undecided); (8.3) verified fact that the manifest-exclude must go through the
  snapshotcontents/<name>/subtree-manifest-identities API-service endpoint (client sdk.SubtreeManifestIdentities,
  server BuildSubtreeManifestIdentities), but the ROOT currently reads archives in-reconciler via
  BuildRootNamespaceManifestCaptureTargets (WithSubresourceREST not wired) — follow-up to migrate the root to
  the endpoint after the frozen-set. Added §3.5 pointer to §8.1.
- **Design** (plan-only, no code) Corrected §3.5 and added §8.4 in docs/content-single-writer-design.md after
  discussion (2026-07-07): distinguished milestone A (content exists/bound — what the write barrier requires)
  from milestone B (status.data.source.uid published, only after the child's VCR completes +
  PublishSnapshotContentDataRef). Key correction: an existing edge (A) does NOT imply materialized
  status.data (B), so the orphan/residual PVC coverage VCR fallback (pvcUIDsFromPendingVCR, spec.targets[].uid)
  MUST be kept for the A->B window; it can be removed only by strengthening the barrier from A to B (edge only
  after status.data published — a stronger, separate gate, not adopted). §8.4 documents current orphan coverage
  (UID-based CollectSubtreeCoveredPVCUIDsFromSnapshot over childrenSnapshotRefs -> boundSnapshotContentName ->
  status.data.source.uid, gated on allDeclaredDomainChildSnapshotsReady, residual = ns PVCs minus covered) and
  the deltas under single-writer/frozen-set/subtreePlanned (walk the frozen childrenSnapshotContentRefs instead
  of re-resolving per child; wave gate may read subtreePlanned but coverage still needs B; fallback stays; fewer
  races/duplicates). Unchanged: coverage stays PVC-UID based; manifest leg (subtree-manifest-identities) stays
  separate.
- **Design** (plan-only, no code) Added §9 "Content creation timing — eager shell" to
  docs/content-single-writer-design.md after decision (2026-07-07, user chose Option A). Documented today's
  lazy creation (GenericSnapshotBinderController gated on isDomainPlanningComplete / phase>=Planned; domain
  child waits parent bound via ResolveParentSnapshotContentOwnerRef; orphan child-volume-node post root-bind)
  and the creation cycle (root content <- root Planned <- children Ready <- children content <- root content,
  edges C1 parent-first / C4 create-at-Planned / C5 wave7 orphan gate). Decision: create the SnapshotContent
  OBJECT as an empty shell eagerly (node exists), decoupled from phase>=Planned — breaks C4, opening the
  deadlock. Separated three per-node events: shell create (eager) / childrenSnapshotContentRefs frozen-set
  (late, §3.5) / status.data (late, post-VCR); the phase>=Planned gate moves off shell-create onto edge-write.
  Immutability stays Option A (set-once from empty; Option B create-time population incompatible with empty
  shells). Added INV-CONTENT-CREATE-1 (content existence implies nothing about plan/readiness/data), risks
  (readers keying on existence-as-Planned, empty-shell GC, snapshotRef handshake), and Slice 0 in §4 (eager
  shell, prerequisite before/with slice 1).
- **Design** (plan-only, no code) Added §3.6 "ChildrenReady read barrier" to docs/content-single-writer-design.md
  after constraint (2026-07-07): ChildrenReady must be computed only after the frozen childrenSnapshotContentRefs
  is committed. Grounded in validateCommonContentChildren (snapshotcontent/controller.go:940-1009), which today
  returns ChildrenReady=True ("no child content") on empty edges — with eager shells (§9) an early empty parent
  shell would flip subtrees Ready prematurely. Fix: treat empty edges + non-empty declared child set as
  ChildrenLinkPending (generalize the orphan-only unlinkedOrphanChildContents gate, controller.go:995-1004, to
  ALL declared children; leaf = declared-empty stays True). No cycle: §3.5 writes edges once children have
  content (A), §3.6 then evaluates each linked child's Ready (B/subtree). Added INV-CONTENT-CHILDREN-4, folded
  the read barrier into Slice 0 (mandatory with eager shells), added a §6 unit test, and updated the
  "Ready model unchanged" invariant to note this one addition.
- **Design** (plan-only, no code) Added §8.5 "Orphan PVC list construction — walk content, VCR fallback via
  xxxSnapshot" to docs/content-single-writer-design.md (proposal 2026-07-07). Target: build residual/orphan PVC
  list by walking the root content's frozen childrenSnapshotContentRefs subtree (skip IsChildVolumeNodeContent);
  per node use status.data.source.uid (milestone B); intermediate node contributes nothing (recurse); leaf
  without data yet -> fallback: resolve content.spec.snapshotRef -> owning xxxSnapshot -> read
  status.captureState.domainSpecificController.volumeCaptureRequestName -> GET VCR -> ParseVolumeCaptureTargets
  -> PVC UIDs. Rationale: this is a CORRECTNESS fix, not cosmetics — today's fallback pvcUIDsFromPendingVCR
  derives the VCR name from the content UID (SnapshotContentVCRName), which matches only content-owned
  (root/orphan) VCRs; a domain data-leaf's VCR is snapshot-owned (SnapshotOwnedVCRName) with its name published
  in captureState, so the content-UID lookup misses it. Reading vcrName off the xxxSnapshot mirrors the binder's
  data-leg projection (domain_content.go:90-104). Noted the walk-source caveat (content-tree walk authoritative
  only post-frozen-set; snapshot-graph ...FromSnapshot still needed in the Late-Planned window) and cross-linked
  §8.4 "VCR fallback" bullet to §8.5.
- **Design** (plan-only, no code) Corrected §8.5 after review (2026-07-07): the "intermediate node (has children)
  => no data, contributes nothing" step was a heuristic, not an invariant. A node may carry BOTH children and a
  data leg (now or future). Data-bearing-ness MUST be decided authoritatively from the CSD field
  spec.requiresDataArtifact (customsnapshotdefinition_types.go:65-69) via the existing accessor
  GVKRegistry.RequiresDataArtifact(kind) (pkg/snapshot/gvk_registry.go:177-179; already used by binder at
  controller.go:425 / import.go:162 / domain_content.go:283), keyed by content.spec.snapshotRef.kind. Rewrote
  the per-node algorithm: (1) not data-bearing -> no covered UID; (2) data-bearing + status.data -> data.source.uid;
  (3) data-bearing + no data yet -> VCR fallback via xxxSnapshot.captureState.volumeCaptureRequestName; ALWAYS
  recurse into children regardless. Explicitly flagged that today's coveredPVCUIDsForContent
  (subtree_covered_pvc.go:144-170) has the `if hasChildren { return nil }` short-circuit that must be dropped.
  Minor caveat noted: accessor keyed by Kind string; widen to full GVK if a Kind collides across apiVersions.
- **Refactor** (w8-block0, code) Block 0 eager content shell + ChildrenReady read barrier — the confirmed fix
  for the pre-Planned orphan-wave demo-tree deadlock (design §9/§9.2 + §3.6). In
  genericbinder/controller.go moved the SnapshotContent object create AND bind (status.boundSnapshotContentName)
  ahead of the isDomainPlanningComplete (phase>=Planned) gate: the shell is now created+bound as soon as the
  Snapshot exists, and the gate moved OFF creation ONTO status projection only (eagerInitCaptureLegs +
  ensureSnapshotContentLinks + checkConsistencyAndSetReady still require Planned). Eager BIND (not just create)
  is required to break C1: a child's ResolveParentSnapshotContentOwnerRef needs the parent content bound. Left
  allDeclaredDomainChildSnapshotsReady untouched (full-Ready gate; relaxed later in Block 5). In
  snapshotcontent/controller.go generalized the fail-closed orphan-link gate in validateCommonContentChildren to
  ALL declared non-leaf children: a parent with declared-but-unlinked children reads ChildrenReady=False /
  ChildrenLinkPending (fail-closed) instead of True-on-empty, so an eager shell cannot flip a subtree Ready
  before its edge set is frozen. Guarded by status.subtreeManifestsPersisted (skip once latched — the same
  declared-vs-linked check already proved linkage; also avoids re-gating a recycle-bin content whose owner is
  gone). Adjusted TestReconcileCommonStatusNotReadyWhileArchivePending to link+Ready the child (isolating the
  lowest-priority subtree-persist gate from the new read barrier) and added an integration spec
  (snapshot_deletion_test.go) proving a Snapshot deleted BEFORE Planned gets an eager Retain shell whose
  parent-protect finalizer is still removed on deletion (no wedge, hazard H7). gofmt + go vet + full controller
  module tests green.
