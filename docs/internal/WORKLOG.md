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
- **Test** (w8-block0, e2e) Block 0 e2e coverage (design §9/§9.2). In e2e/tests/capture_test.go annotated the
  existing "captures the demo snapshot tree" spec as the explicit regression guard for the pre-Planned
  orphan-wave deadlock (the exact create cycle root content <- root Planned <- children Ready <- child content
  bound <- root content that the eager shell breaks) — if Block 0 regresses, that Ready wait times out. In
  e2e/tests/namespace_capture_rbac_test.go added eagerShellDeletionSpecs() (wired into
  namespaceCaptureReworkSpecs) proving the no-wedge invariant on a live cluster: a root Snapshot created and
  immediately deleted (best-effort pre-Planned) is fully GC'd — the eager Retain shell's parent-protect
  finalizer never wedges the Snapshot's deletion. Deterministic pre-Planned timing stays pinned by the
  integration spec (test/integration/snapshot_deletion_test.go); the live cluster's Planned transition is too
  fast to pin, so the e2e asserts the timing-robust invariant. gofmt + go build + go vet (e2e module) green.
- **Bugfix** (w8-block0, test) Fixed the Block 0 eager-shell deletion integration spec
  (test/integration/snapshot_deletion_test.go): dropped the `spec.deletionPolicy == "Retain"` assertion on the
  eager shell. The integration harness registers a minimal TestSnapshotContent CRD whose schema has no
  spec.deletionPolicy, so the API server prunes the field and the assertion always failed once the spec ran
  under envtest (it was not exercised in Block 0). The durable Retain policy is covered on the real common
  SnapshotContent GVK in the binder unit path; hazard H7 only needs the shell to exist and never wedge deletion,
  which the spec still asserts.
- **Refactor** (w8-block1, code) Block 1 single-writer child content edges -> aggregator (design §3.1/§3.2,
  INV-CONTENT-CHILDREN-1). Moved child-edge projection into the SnapshotContentController: new
  reconcileChildContentEdges runs at the top of Reconcile, resolves the owning snapshot via spec.snapshotRef
  (ownerChildrenSnapshotRefs), and projects status.childrenSnapshotContentRefs from the owner's
  status.childrenSnapshotRefs (PublishSnapshotContentChildrenFromSnapshotRefs, append-only, all-or-nothing;
  requeues via `!ready || edgesRequeue`). A steady-state short-circuit skips the per-child uncached resolution
  once len(edges) >= len(childRefs) to avoid hammering the apiserver on the 500 ms readiness self-requeue.
  Removed the external DOMAIN/generic/import edge writers: genericbinder/domain_content.go (+ its now-unused
  parseChildrenSnapshotRefs), genericbinder/import.go, snapshot/import.go — the binder stays the creator/binder
  of the child content objects + parent ownerRefs, it no longer writes the edge set. Extracted the shared
  raw->[]SnapshotChildRef parse into snapshotChildRefsFromRaw (reused by the orphan-link read barrier) and
  fixed a pre-existing gci import-order finding in snapshotcontent/controller.go. DEVIATION from the plan's
  Block 1 scope: the ORPHAN volume-node leaf edge (LinkChildVolumeContentRef in
  snapshot/orphan_pvc_volume_snapshot.go) stays in the snapshot path until Block 3, co-located with orphan
  content CREATION (EnsureVolumeChildContent) — splitting create/link across controllers regressed orphan-wave
  convergence in the integration suite. The aggregator already RESOLVES the orphan child name for the
  ChildrenReady READ barrier, so this is only about WHERE the write lives; both writers are append-only under an
  optimistic lock. gofmt + go vet + golangci-lint (changed files) + unit tests + two-pass make test-integration
  all green; Bugbot found no bugs.
- **Test** (w8-block1, e2e) Block 1 e2e coverage (design §3.1/§3.2, INV-CONTENT-CHILDREN-1). Extended
  e2e/tests/capture_test.go captureSpecs with a spec asserting single-writer edges: for every snapshot node in
  the manifest-only tree (root + descendants), its bound content's status.childrenSnapshotContentRefs equals
  EXACTLY the multiset of bound-content names of its declared NON-LEAF children (childrenSnapshotRefs minus CSI
  VolumeSnapshot visibility leaves) — no missing edge, no duplicate (counts asserted == 1). Added local helpers
  declaredChildContentNames (expected set from the owning snapshot) and contentChildEdgeNames (actual multiset
  from the content), kept in capture_test.go. gofmt + go vet (e2e module) green; Bugbot found no bugs. Full run
  needs a cluster.
- **Refactor** (w8-block2, code) Block 2 single-writer manifestCheckpointName -> aggregator (design §3.1/§3.2,
  INV-CONTENT-WRITER-1). Moved status.manifestCheckpointName projection OFF the binder AND the namespace domain
  into the SnapshotContentController: new reconcileManifestCheckpointNameProjection reads the owning snapshot's
  status.captureState.domainSpecificController.manifestCaptureRequestName -> MCR -> mcr.status.checkpointName and
  publishes it (PublishSnapshotContentManifestCheckpointName) before fillOwnLegs reads it. The projection is
  latch-idempotent: once published, a post-handoff MCR NotFound (the binder reaps the MCR after the durable MCP
  ownership handoff) keeps the pointer and does not requeue; a pre-publish NotFound/empty checkpointName
  requeues (also backstopped by the 500 ms !ready self-requeue). Removed the two root/domain writers:
  genericbinder/controller.go ensureSnapshotContentLinks (kept the mcrName read only to drive the domain
  request lifecycle) and snapshot/capture.go driveRootManifestCheckpointReadiness (+ its now-unused
  snapshotcontent import; the local mcpName is still derived to read the MCP directly). Added the aggregator's
  MCR watch (mapMCRToBoundContent, wired into addSnapshotStatusWatchLocked): enqueue-only, maps MCR ->
  owning-snapshot-of-GVK (List+filter on the same manifestCaptureRequestName truth ref) ->
  status.boundSnapshotContentName, making the checkpointName handoff event-driven (mirrors the binder's
  mcr_watch.go). Consolidated the per-pass owner resolve into ownerSnapshot (shared by both the child-edge and
  manifest projections; replaces ownerChildrenSnapshotRefs) and SKIP it entirely for child-volume-node leaf
  contents so no owner Get is added to the load-sensitive orphan-wave reconcile path. DEVIATION from the plan's
  Block 2 scope: the ORPHAN child-volume-node manifest publish (snapshot/orphan_pvc_volume_snapshot.go
  ensureOrphanVolumeChildManifestCheckpoint) and the import-owner publishers stay put — the orphan publish is
  co-located with the per-orphan MCR create/delete lifecycle gated on content.status.manifestCheckpointName
  (splitting only the publish regresses convergence, same reason Block 1 kept the orphan EDGE), and both move in
  Block 3/6 respectively. Fixed the N5 PR-7 integration specs to assert the durable ManifestCheckpoint (via the
  content's manifestCheckpointName) when the now-transient root MCR has already been GC'd (the aggregator's
  atomic name-publish + MCP handoff collapses the observation window). gofmt + go build + go vet +
  golangci-lint (no new findings) + unit tests + isolated (3x) and non-isolated make test-integration all green;
  Bugbot found no bugs.
- **Test** (w8-block2, e2e) Block 2 e2e coverage (design §3.1/§3.2, INV-CONTENT-WRITER-1). Extended
  e2e/tests/capture_test.go captureSpecs with a spec asserting the single-writer manifest leg end-to-end: for
  every snapshot node in the manifest-only tree (root + descendants, excluding CSI VolumeSnapshot visibility
  leaves) its bound content publishes a non-empty status.manifestCheckpointName, the referenced cluster-scoped
  ManifestCheckpoint is Ready=True, AND that MCP carries an ownerReference back to the SAME content (the durable
  ownership handoff that lets the MCP be GC'd with the content). Walk runs under Eventually with a non-empty +
  DemoVirtualMachineSnapshot inventory guard so the per-node assertions can't pass vacuously during the
  eager-shell childrenSnapshotRefs materialization window (mirrors the sibling childrenSnapshotRefs spec). Added
  local helper ownedBySnapshotContent (ownerRef predicate); reuses walkSnapshotTree/gvrForSnapshotKind/
  conditionStatus. gofmt + go vet (e2e module) green; Bugbot found no bugs (flagged the vacuous-walk window on
  first pass, fixed, clean on re-review). Full run needs a cluster.
- **Update** (w8-design, docs) docs/content-single-writer-design.md — new §11 "VolumeSnapshot domain"
  (decision 2026-07-07): the forked CSI VolumeSnapshot (v1 only) becomes a CSD-registered domain snapshot
  kind driven by a dedicated reconciler in storage-foundation via pkg/snapshotsdk; scope is every NEW
  VolumeSnapshot incl. user-created (standalone = one-node tree, d8-exportable); old/new discriminator =
  fork "taken into work" label stamped in syncUnreadySnapshot; veto = ExcludeLabelKey on the source PVC
  only, latched via a managed label; manifest leg = standard EnsureManifestCapture(PVC), data leg = native
  CSI (aggregator projects from owner.status.boundVolumeSnapshotContentName -> VSC, binder writes the
  dataCaptured latch); CSD-registered kinds are domain-capture by definition. §11.6 enumerates the orphan
  machinery to dismantle (visibility leaves, child-volume-node contents, per-orphan MCR/MCP,
  bindOrphanVSToChildContent, data_readiness special case). Marked the superseded orphan carve-outs across
  §3.1/§3.5/§3.6/§4 (Slices 1-3)/§5/§6/§7/§8.1/§8.4/§8.5/§9 with pointers to §11; extended the milestone-B
  and A->B-fallback wording for native-CSI kinds (snapshotSource-based covered-UID). Doc-only change; code
  lands per the re-cut Blocks 3/3b/3c/3d.
- **Update** (w8-design, docs) Final cross-doc review fix (plan/design/ADR consistency pass): §3.3 now
  records the as-executed Block 1 deviation — LinkChildVolumeContentRef stays (two append-only edge
  writers) until the §11.6 dismantling (plan Block 3d), so the §3.4 immutability CEL must not land before
  it. Doc-only.
- **Refactor** (w8-block3, code) Block 3 single-writer data leg -> aggregator + restore re-point -> binder +
  CSD-implies-domain-capture (design §4 Slice 3 / §11.4/§11.5, INV-CONTENT-WRITER-1). New
  snapshotcontent/datarefs_projection.go: reconcileDataLegProjection is now the SOLE writer of
  SnapshotContent.status.data for domain owners — it projects the captured volume artifact from the owning
  snapshot's VolumeCaptureRequest (VCR domains: captureState.domainSpecificController.volumeCaptureRequestName
  -> VCR -> VolumeSnapshotContent) or, for a native-CSI kind VolumeSnapshot owner, from its bound VSC
  (owner.status.boundVolumeSnapshotContentName + status.snapshotSource; dormant until the foundation CSD
  registers the kind in Block 3c), doing the enrich + VSC Retain/ownerRef handoff + status.data publish that
  the binder used to do. Wired into SnapshotContentController.Reconcile before the status aggregation; no VCR
  watch added (uncached APIReader read + the 500 ms self-requeue drive convergence, design §3.2). The binder
  (genericbinder/domain_content.go) is now READ-ONLY over the VCR: it keeps only the VCR-lifecycle terminal
  failure surfacing (Ready=False) + the commonController.dataCaptured latch + transient-VCR reaping once the
  aggregator's status.data covers the targets, and a symmetric dataCaptured latch for the native-CSI
  VolumeSnapshot branch (fires once content.status.data present). Removed the namespace-domain non-residual
  data publish in snapshot/volume_capture.go (dead for named roots; residual/orphan publish stays until Block
  3d). Restore spec.snapshotRef re-point moved off the namespace domain (snapshot/static_bind.go) into the
  binder (genericbinder/static_bind.go repointContentSnapshotRefToSelf) — the binder is the creator and sole
  writer of content.spec, re-pointing a recycle-bin (status.parentDeleted) content onto the re-created
  StaticBind CR under the relaxed-CEL gate. unifiedruntime/syncer.go marks every CSD-derived kind outside the
  SS-internal dedicated lists as domain-capture by definition. Review fixes: (1) the domain StaticBind
  re-point mismatch is handled non-terminally with a poll requeue (a not-yet-observed parentDeleted must not
  terminal-stop a not-yet-bound restore CR, which the content->snapshot reverse map would never re-enqueue);
  (2) closed a same-pass premature Ready — reconcileDataLegProjection surfaces a dataLegPending signal that
  reconcileCommonSnapshotContentStatus uses to downgrade the stale-empty volume leg (empty status.dataRefs
  reads as volume N/A) to DataCapturePending and re-derive Ready, so Ready cannot escalate (and mirror to the
  owner) before the bound VSC's readyToUse is validated on the next pass. DEVIATION (plan Block 3 scope): the
  orphan machinery and its content-status writes (orphan_pvc_volume_snapshot.go data + per-orphan MCR/MCP,
  child-volume-node contents) STAY until Block 3d; INV-CONTENT-WRITER-1 becomes STRICT only then. gofmt +
  go build + go vet + golangci-lint (--new-from-rev=HEAD: no new findings) + unit tests all green; Bugbot
  found two issues (restore terminal-stop, premature Ready), both fixed and clean on re-review.
- **Test** (w8-block3, e2e) Block 3 e2e data-leg regression (E2E_VOLUME_DATA): extended volumedata_test.go
  with an It that BFS-walks the captured content tree and asserts every domain data leg landed the
  aggregator's durable VolumeSnapshotContent handoff — spec.deletionPolicy=Retain (survives the transient
  VolumeCaptureRequest being reaped) plus an ownerReference back to its SnapshotContent — covering at least
  the two domain-disk VCR legs (the legacy orphan leaf does the same handoff on the snapshot path until Block
  3d, so it is asserted too when present). Added volumeSnapshotContentGVR to e2e_shared_test.go. Compile-check
  only (gofmt + go vet, no cluster at night per the pre-approval); pre-existing pending-VCR observer WIP in
  both files left intact. Bugbot: no bugs.
- **Add** (w8-block3b, storage-foundation) Extended CSI VolumeSnapshot v1 as a state-snapshotter domain
  snapshot kind (design §11.1/§11.3). Lands in the storage-foundation repo (branch api-rework, commit
  39987ed), not state-snapshotter. CRD (crds/snapshot.storage.k8s.io_volumesnapshots.yaml): the stable v1
  status gains captureState (commonController + domainSpecificController latches/refs), childrenSnapshotRefs,
  snapshotSource, and conditions — mirroring the reference domain snapshot schema; together with the
  pre-existing boundSnapshotContentName + conditions this satisfies the CSD CRD contract. The legacy v1beta1
  version is stripped of every fork field (spec.source.import, status.boundSnapshotContentName, status.data),
  stays storage=false, and is therefore never treated as a domain object. Fork
  (images/snapshot-controller/patches/003-volumesnapshot-dataimport-fork.patch): the external-snapshotter
  client VolumeSnapshotStatus gains the matching CaptureState/ChildrenSnapshotRefs/SnapshotSource/Conditions
  fields + supporting types + deepcopy so UpdateStatus round-trips the domain-written fields instead of
  dropping them; syncUnreadySnapshot stamps the storage-foundation.deckhouse.io/processed label (idempotent
  check-then-set, JSON-pointer-escaped) as the fork-label discriminator that lets the VS domain controller
  adopt NEW managed VolumeSnapshots while leaving pre-existing legacy ones alone (import VSs never reach this
  path and are never labeled). Verified: CRD YAML parses with the expected per-version key sets; the
  regenerated patch applies cleanly on a pristine fork tag and the patched tree builds + vets. Bugbot: 1
  Medium — the CEL that permits an empty spec.source ("restore intent") while syncSnapshot still rejects
  both-nil PVC/VSC as SnapshotValidationError; confirmed pre-existing (committed in HEAD before this block,
  untouched by the Block 3b diff), so it is out of scope here and deferred to the import/restore wiring
  (Block 6).
- **Add** (w8-block3d, binder domain-claim gate) The generic binder now gates its eager ObjectKeeper +
  SnapshotContent shell for a domain-capture kind on the domain having CLAIMED the object — i.e. having
  written any part of status.captureState.domainSpecificController (new domainHasClaimed helper in
  genericbinder/domain_content.go; gate in controller.go Reconcile, after the import/StaticBind branches and
  before the eager shell). This lets a domain expose only a SUBSET of a CSD-registered kind as domain
  objects: the storage-foundation VolumeSnapshot domain (Block 3c) skips legacy/unlabeled, vetoed,
  import-mode, and pre-provisioned VolumeSnapshots (§11.3), so those are never claimed and the binder leaves
  them entirely untouched (a plain CSI snapshot, no content, no ObjectKeeper) instead of shelling every
  VolumeSnapshot in the cluster once the CSD registers the kind. Closes the Block 3c Bugbot HIGH finding
  ("CSD activates binder without domain gate"), which was dispositioned to the state-snapshotter side per the
  creator/main split (plan decision #4, design §11.3/§11.6). Deadlock-safe (design §9): the claim is written
  on the domain's first reconcile independent of the content existing — for the namespace root the step-3
  EnsureChildren claim precedes the step-4 orphan-wave Ready gate (verified in namespace_capture_run.go) — so
  it is strictly earlier than the phase>=Planned projection barrier and the eager-shell creation cycle stays
  broken. Validated: gofmt + go build + genericbinder unit tests + the FULL envtest integration suite
  (root-lifecycle / demo-tree / orphan deadlock scenarios) all green. First landing of Block 3d; the orphan
  special-path dismantling (INV-CONTENT-WRITER-1 STRICT) follows.
- **Remove** (w8-block3d, orphan special-path dismantling) Deleted the orphan "child volume node" /
  "VolumeSnapshot visibility leaf" special path so an orphan/residual-PVC VolumeSnapshot is an ORDINARY
  domain child end to end (content-single-writer design §11.6, INV-CONTENT-WRITER-1 STRICT): its content
  shell is created + bound by the generic binder and ALL of that content's status (data from the bound VSC,
  manifestCheckpointName from the VS domain's manifest leg, childrenSnapshotContentRefs, Ready) is projected
  by the aggregator — the namespace domain writes NO SnapshotContent. Rewrote
  snapshot/orphan_pvc_volume_snapshot.go to create each residual-PVC CSI VolumeSnapshot (explicit
  StorageClass→volumesnapshotclass annotation class resolution, validated against the bound PV CSI driver;
  terminal class failures degrade the Snapshot's own Ready) and declare them via the SDK EnsureChildren
  (additive childrenSnapshotRefs union, excluded set re-passed). Removed the pre-Planned step-8 orphan
  child-content linking from namespace_capture_run.go; dropped the visibility-leaf / child-volume-node
  carve-outs from the aggregator (snapshotcontent/controller.go, datarefs_projection.go, status_publish.go),
  the plan/parent/child-graph gates (namespace_capture_plan.go, parent_graph.go,
  child_snapshot_terminal_failures.go), the subtree coverage walks (subtree_covered_pvc*.go), and the root
  manifest-exclude builder (root_capture_run_exclude.go). Deleted the dead legacy machinery
  (snapshot/volume_capture.go, snapshotcontent/volume_child_content.go, and the whole reconcileCaptureN2a
  cluster in snapshot/capture.go) plus the removed leaf helpers/label from pkg/snapshot (visibility_leaf.go
  renamed → csi_kinds.go, now CSI constants only: IsVolumeSnapshotVisibilityLeaf / IsChildVolumeNodeContent /
  LabelChildVolumeNode gone). Removed the orphan-volume-leaf restore special case from static_bind.go
  (orphan/standalone VolumeSnapshot children are SKIPPED by the recycle-bin static-bind walk; their restore
  flows through the unified import model in Block 6). Test rewrites: deleted obsolete
  orphan_link_gate_test.go / volume_capture_test.go / volume_child_content coverage, added
  orphan_pvc_volume_snapshot_test.go (class-resolution terminal/transient cases), and reoriented the
  visibility-leaf-skip unit tests to the new "VS is an ordinary domain child" semantics (gate closes on an
  uncreated VS child, coverage recurses into it, content-child publish requeues an unbound VS). Rewired the
  N5 PR-7 envtest simulator (n5_pr7_csi_simulator.go) to play BOTH missing roles — the external-snapshotter
  CSI sidecar AND the storage-foundation VolumeSnapshot domain controller (claim + snapshotSource + manifest
  leg) — so the orphan wave completes as a domain child; observable helper switched from the label-scoped
  child-volume-node to the orphan VS's bound content dataRef. Validated: gofmt clean, go build ./... +
  go vet ./... (incl. -tags integration) clean, full module unit suite green. Live envtest/e2e validation of
  the rewired n5_pr7 simulator is deferred to the end-of-plan validation pass (per the compile-only overnight
  pre-approval).
- **Bugfix** (w8-block3d, review cycle) Fixed two Bugbot HIGH findings on the orphan-dismantling diff.
  (1) orphanPVCVolumeSnapshotSpecMismatch no longer re-validates spec.volumeSnapshotClassName once the
  pre-existing orphan VolumeSnapshot is already BOUND (status.boundVolumeSnapshotContentName set): a durable
  snapshot exists, the class has served its only purpose (driver/params at creation), so re-resolving it
  (e.g. an operator edits the StorageClass storage.deckhouse.io/volumesnapshotclass annotation between
  reconciles) MUST NOT flip an already-captured snapshot to terminal VolumeCaptureFailed — restores the
  pre-dismantling validateExistingOrphanPVCVolumeSnapshot semantics; the source PVC stays fail-closed even
  when bound (misattributing another PVC's data is a real fault). (2) The Snapshot-graph coverage walk
  (CollectSubtreeCoveredPVCUIDsFromSnapshot / walkSnapshotChildRefsForCoverage) now SKIPS a
  referenced-but-absent CSI VolumeSnapshot child instead of failing closed: it is the residual wave's own
  deterministically-named (rootUID, pvcUID) output, so skipping re-classifies its PVC as residual and the
  wave recreates it at the same name (idempotent) — failing closed wedged the wave, since coverage errors
  requeue in ensureOrphanVolumeSnapshotsPrePlanned before EnsureChildren could recreate the object. Every
  other missing child stays fail-closed (not self-recreating). Unit tests added for both (bound-handle class
  drift tolerated / wrong source still terminal; absent-orphan-VS self-heal). Bugbot MEDIUM (stale orphan
  refs never pruned) dispositioned by-design: the plan explicitly removed the VS-partition maintenance
  (reconcileOrphanPVCVolumeSnapshotChildLeaves) in favor of additive EnsureChildren, orphan VS is now an
  ordinary domain child (same additive-ref property as all domain children), and the residual set is
  deterministic after the frozen MarkPlanned barrier — the only shrink path is a PVC deleted mid-capture, a
  general child-lifecycle edge, not orphan-specific. Re-ran Bugbot: no findings. gofmt + build + vet
  (incl. -tags integration) + full unit suite green.
- **Add** (w8-block3d, e2e) VolumeSnapshot-domain e2e coverage for the content-single-writer orphan dismantling.
  New e2e/tests/volumesnapshot_domain_test.go (env-gated E2E_VOLUME_DATA, registered in the suite): (a) a
  USER-created standalone CSI VolumeSnapshot is adopted by the storage-foundation VS domain controller
  (fork storage-foundation.deckhouse.io/processed label + state-snapshotter.deckhouse.io/managed=true), the
  core binder creates+binds its state-snapshotter SnapshotContent (status.boundSnapshotContentName), the
  aggregator projects the manifest leg (manifestCheckpointName -> Ready MCP owned by the content) and the
  native-CSI data leg (status.data VolumeSnapshotContent, Retain + owned), conditions[Ready] is mirrored back,
  and the VS connector manifests-download returns the one-node tree incl. the source PVC manifest; (b) a
  VolumeSnapshot on an exclude-vetoed PVC latches managed=false, stays a plain CSI snapshot (bound
  boundVolumeSnapshotContentName), and Consistently owns NO ManifestCaptureRequest and gets NO
  state-snapshotter SnapshotContent. Also added an It to volumedata_test.go asserting the phase-3 root
  orphan/residual PVC (demo-pvc) is now an ordinary VolumeSnapshot domain child end to end (same adopt ->
  content -> manifest+data legs -> Ready pipeline), proving INV-CONTENT-WRITER-1 (content created by the binder,
  not the ns domain). Review cycle (Bugbot HIGH x1 + MEDIUM x1): sized every spec's parent ctx to the sum of
  its sequential per-step waits (N*perStepTO+buffer idiom) and budgeted the orphan-VS adoption pipeline at the
  generous snapshotReadyTO (multi-controller convergence), NOT the 30s captureReadyTO (pure snapshot creation),
  matching the parallel user-VS pipeline. Re-ran Bugbot: no findings. e2e test binary compiles (go test -c);
  live-cluster run deferred to the end-of-plan validation pass (compile-only per the overnight pre-approval).
- **Update** (w8-block4) SnapshotContent.status.childrenSnapshotContentRefs is now an IMMUTABLE FROZEN SET
  (INV-CONTENT-CHILDREN-2, Option A CEL). api/storage/v1alpha1: replaced the append-only transition rule
  (self.size() >= oldSelf.size()) with oldSelf.size() == 0 || self == oldSelf — the empty->complete first
  write is the only allowed change; once non-empty the set is immutable (no add, remove, reorder, or replace).
  Bounded the CEL estimated cost so the apiserver accepts the CRD: +kubebuilder:validation:MaxItems=8192 on
  the list and MaxLength=253 (DNS-subdomain object-name ceiling) on SnapshotContentChildRef.Name — the O(n)
  self==oldSelf comparison over an unbounded list/name would blow the cost budget and reject the CRD (this is
  why the old O(1) size rule capped nothing). This is safe only because the aggregator became the SOLE,
  all-or-nothing edge writer in Block 3d: PublishSnapshotContentChildrenFromSnapshotRefs publishes the
  COMPLETE declared-child set in one transition (it early-returns until every declared child is bound), so the
  field goes empty->complete in a single patch and never grows incrementally. Made the writer frozen-aware
  (status_publish.go PublishSnapshotContentChildrenRefs): added a guard that HOLDS an already-populated set
  as-is instead of attempting to grow/replace it — this is a no-op on fresh deployments (every later pass
  recomputes the identical complete set, incl. the E3-degraded re-publish) and makes the writer upgrade-safe
  against a legacy partial set carried over from the append-only era (completing it would be CEL-rejected and
  wedge the reconcile in ChildrenLinkPending). Refreshed the frozen-one-shot semantics comments in
  controller.go/status_publish.go. Tests: new integration snapshotcontent_children_frozen_test.go pins the
  admission contract (accepts empty->complete + idempotent identical re-write; rejects shrink/append/reorder/
  replace), a new writer-level unit test pins the frozen-set hold guard, and the integration seed helper was
  refactored to a batch mergeChildrenGraphIntoRoot (single atomic complete-set write) since seeding children
  one-by-one now violates the CEL (n5_pr7 two-PVC updated to seed both children atomically). Review cycle
  (Bugbot HIGH: frozen CEL wedges partial edge sets) fixed by the writer frozen guard; re-ran Bugbot: no
  findings. gofmt + build + vet (incl. -tags integration) + snapshotcontent unit suite + isolated
  frozen-set integration test all green; CRD regenerated (hack/generate_code.sh).
- **Refactor** (w8-block4, tests) Deferred the two reactor-driven orphan-domain-child specs in
  n5_pr7_two_pvc_integration_test.go to Block 5 (PIt). They exercise the full residual-PVC -> VolumeSnapshot
  domain pipeline (orphan wave creates the VS, the storage-foundation VS domain controller adopts+plans it,
  the binder creates+binds its SnapshotContent, the aggregator projects data+manifest, the root subtree
  barrier clears), which is not yet closed at the integration level after the Block 3d model rewrite:
  registering the production CSD (source PVC -> VolumeSnapshot) makes the namespace planner enumerate the
  residual PVCs and create VolumeSnapshot children as GENERIC shells (no spec.source.persistentVolumeClaimName)
  the domain reactor cannot adopt, so the orphan content is never created and the root manifest leg hangs.
  Reconciling the namespace-planner-vs-orphan-wave overlap + domain-capture VolumeSnapshot spec population is
  exactly Block 5's scope (these specs never passed after the Block 3d rewrite). The duplicate-covered-PVC-UID
  guard spec in the same suite needs no orphan pipeline and stays active; the isolated suite is green.
- **Add** (w8-block4, e2e) Frozen-set stability detector to e2e/tests/ready_flap_test.go. On the mixed
  orphan+domain vol-tree capture, a third watch recorder (opened BEFORE the root content exists, alongside
  the existing Ready + ChildrenReady recorders) records every distinct value of the root SnapshotContent's
  status.childrenSnapshotContentRefs via a sorted, order-independent set signature. New assertChildrenRefsFrozen
  latches the first non-empty set and fails on ANY later differing sample — a grow, a shrink (incl. back to
  empty), a reorder-as-different-membership, or a replace — proving on-cluster that the sole all-or-nothing
  writer (the aggregator) publishes the complete child set exactly once (empty -> complete) and never mutates
  it through the whole Ready convergence (Block 4, INV-CONTENT-CHILDREN-2). This is the runtime counterpart to
  the CEL admission test (snapshotcontent_children_frozen envtest); the CEL rejection itself is not duplicated
  on-cluster. Bugbot: no findings. gofmt + go vet + go test -c (full e2e binary) green; live-cluster run
  deferred to the end-of-plan validation pass (compile-only per the overnight pre-approval).
- **Refactor** (w8-block5) Orphan/residual PVC coverage now decides data-bearing-ness AUTHORITATIVELY from the
  CSD (snapshot.GVKRegistry.RequiresDataArtifact via a new volumecapture.DataBearingKindFunc predicate keyed on
  the owning snapshot kind), replacing the shape-of-the-tree heuristic (coveredPVCUIDsForContent dropped the
  `if hasChildren { return nil }` short-circuit — a kind may legitimately carry BOTH children and a data leg,
  design §8.5). For the A->B window (a data-bearing node whose status.data is not published yet) coverage now
  falls back through the OWNING snapshot resolved from content.spec.snapshotRef, not a content-UID-derived VCR:
  VCR-based domains read status.captureState.domainSpecificController.volumeCaptureRequestName and take that
  VCR's spec.targets[].uid (new pvcUIDsFromNamedVCR / coveredPVCUIDsFromOwnerFallback), and native-CSI
  VolumeSnapshots (no VCR) take the owner's status.snapshotSource.uid (§11.7). The predicate is threaded through
  both coverage walks (subtree_covered_pvc.go content-tree + subtree_covered_pvc_from_snapshot.go snapshot-graph)
  and all their consumers (ListOwnedPVCTargetsForLogicalContent, listResidualRootOwnedPVCTargets,
  OwnedPVCManifestTargetsForSnapshot, usecase.BuildRootNamespaceManifestCaptureTargets); the two reconciler call
  sites source it from the live registry via a new SnapshotReconciler.dataBearingKindFunc() that fails closed
  (ErrGraphRegistryNotReady -> requeue) exactly like buildSnapshotMachineryGVKs, so an empty registry never
  under-covers (which would let an already-captured PVC be re-captured). Review cycle (Bugbot HIGH: relaxed
  phase>=Planned gate opens before coverage is observable) fixed two ways: (1) the snapshot-graph walk now
  reads the owner fallback DIRECTLY off the child snapshot's own captureState (new coveredPVCUIDsForSnapshotNode
  + coveredPVCUIDsFromOwnerObject) instead of requiring a bound content first — a Planned-but-not-yet-bound
  child is now covered via its in-flight VCR name / snapshotSource.uid; (2) a data-bearing node with NO
  observable coverage yet now returns ErrSubtreeDataRefsPending (previously defined but never returned; both
  call sites already treat it as a requeue), so the residual wave WAITS for a still-Planning descendant rather
  than under-covering (the relaxed gate only guarantees DIRECT children are Planned). Re-ran Bugbot: clean.
  Unit tests: replaced the content-UID VCR coverage test with an owner-VCR-fallback test, added not-data-bearing
  skip + owner-VCR + snapshotSource + pending tests (modeling aggregator vs data-leaf via distinct kinds),
  threaded a permissive allKindsDataBearing predicate through the existing dataRefs-path tests. build + vet +
  gofmt + usecase/snapshot unit suites + full -tags integration test compile all green.
- **Refactor** (w8-block5) Relaxed the orphan/residual-PVC wave barrier allDeclaredDomainChildSnapshotsReady
  from full Ready=True to capture barrier 1 (status.captureState.domainSpecificController.phase >= Planned),
  mirroring weightLayerCaptureReady (childCaptureAtLeastPlanned, no observedGeneration gate — the domain phase is
  monotonic and spec is immutable). This is what the Block 5 owner-fallback unlocks: coverage no longer needs a
  child's status.data (milestone B), only its VCR name / snapshotSource which are published by Planned, so the
  gate can open at Planned. Waiting for full Ready here was the deadlock — a child cannot go Ready until its
  content subtree closes, which needs the root content, which needs this gate. Updated the childrefs_merge_test.go
  gate fixtures (domainChildReady/readyVSChild) to stamp the capture phase instead of the Ready condition. A
  phase=Failed child keeps the gate closed (pending) as before; the terminal is surfaced separately by content
  aggregation (ChildrenFailed).
- **Add** (w8-block5 e2e) e2e/tests/volumedata_test.go: new phase-3 spec "captures each source PVC by exactly
  one data leg" that walks the whole SnapshotContent tree and asserts every source PVC (demo-pvc orphan native-CSI
  leaf + demo-pvc-disk/demo-pvc-standalone domain-VCR legs) is backed by EXACTLY ONE status.data leg. This is the
  observable end-to-end form of the Block 5 orphan-coverage rewrite: exactly-once == no under-coverage
  (fail-closed ErrSubtreeDataRefsPending waits out a still-Planning descendant) AND no duplicate orphan capture
  (data-bearing coverage reads + owner fallback keep a domain-covered PVC out of the residual wave). Complements
  the existing exclusion spec (PVCs absent from the root own-manifests). Compile-check only (go test -c); live run
  deferred to end-of-plan validation.
- **Add** (w8-block6a) Import-MCP durability backstop (content-single-writer design §10.1): the reconstructed
  (import) ManifestCheckpoint was created ownerless at upload time (a cluster-scoped MCP cannot be owned by the
  namespaced snapshot, and the eager SnapshotContent shell may not exist yet), leaving a crash/delete window that
  orphaned the MCP + chunks with no sweeper. Fix: at upload the MCP is now born owned by a DEDICATED ObjectKeeper
  that FollowObjects the import snapshot, so it is GC-safe from birth. New names.ImportManifestCheckpointObjectKeeperName
  (distinct nss-import-ok- prefix so it never collides with the snapshot's root ObjectKeeper, which is keyed by the
  same UID) + new usecase.EnsureReconstructedManifestCheckpointObjectKeeper (idempotent create, returns the
  controller ownerRef). The reconstructed-label key is hoisted to an exported constant
  (usecase.ReconstructedManifestCheckpointLabelKey). The aggregator's MCP handoff
  (ensureManifestCheckpointOwnedByContent) now, once it re-parents a RECONSTRUCTED MCP onto its SnapshotContent,
  deletes the now-redundant import ObjectKeeper (gated on the reconstructed label + the reconcile that actually
  performed the handoff, so a capture MCP's execution OK — GC'd with its MCR — is never touched). Unit tests:
  names collision/determinism, EnsureReconstructed... idempotency + FollowObject wiring, upload owns MCP by the
  import OK + carries the reconstructed label, aggregator deletes the import OK after handoff and KEEPS a
  capture-execution OK (reconstructed-gate isolation). Also registered deckhouse.io ObjectKeeper in the api
  connector test scheme (production already registers it in cmd/main.go's api-server fullScheme). build + vet +
  gofmt + full module unit suite green. Review cycle (Bugbot): (HIGH) the controller ClusterRole had no
  `delete` verb on objectkeepers (comment even said "never deletes"), so the §10.1 cleanup Delete would be
  Forbidden and silently swallowed at V(1) — granted `delete` on deckhouse.io/objectkeepers in
  templates/controller/rbac-for-us.yaml with a scoped comment (import-MCP keeper only). (MEDIUM) cleanup was
  gated on the reconcile that performed the handoff, so a handoff completed on a prior pass (or interrupted by a
  crash before the delete) never swept the keeper — re-gated on the MCP being content-owned NOW (idempotent
  delete, so later passes sweep it). (MEDIUM) ReconstructManifestCheckpoint returned early on an already-Ready
  MCP without anchoring it, so a repeat upload against a pre-§10.1 ownerless Ready MCP left an import OK that
  owned nothing — added adoptReadyReconstructedManifestCheckpoint (adopt onto the passed import OK + backfill the
  reconstructed label ONLY when the MCP has no controller owner, never re-anchoring a handed-off MCP). Re-ran
  Bugbot: one follow-up MEDIUM (the not-yet-Ready RESUME path — an existing pre-§10.1 ownerless checkpoint
  finished Ready without anchoring) fixed by generalizing the helper to ensureReconstructedManifestCheckpointAnchored
  and calling it both on the already-Ready early return AND after the create/resume re-get. Added unit tests for
  all four (prior-handoff sweep, Ready-unanchored adopt, not-Ready resume anchor, no-reanchor-when-owned).
- **Refactor** (w8-block6b-1) Import manifest leg → aggregator single writer (content-single-writer design §10):
  the SnapshotContentController is now the sole writer of status.manifestCheckpointName for import too, not just
  capture. New reconcileManifestCheckpointNameProjection import branch (usecase.IsUnstructuredImportMode(owner)
  -> projectImportManifestCheckpointName) projects the deterministic reconstructed checkpoint name
  (usecase.ReconstructedManifestCheckpointName keyed to the owner UID) once the upload endpoint has created it,
  with the same durable-latch semantics as the capture MCR branch (pre-publish NotFound requeues; post-handoff
  NotFound keeps the pointer). Removed the now-duplicate PublishSnapshotContentManifestCheckpointName calls from
  all three import controllers (snapshot/import.go root, genericbinder/import.go leaf, volumesnapshotimport VS);
  they still wait for the checkpoint to exist (VS import also for it to go Ready, to recover the orphan PVC for
  the dataRef) but no longer publish the pointer. No deadlock: the binders publish their data leg and the
  aggregator publishes the manifest leg, both feeding content Ready which the snapshots mirror. Unit tests added
  (manifest_projection_import_test.go: import publishes reconstructed name; pending when no checkpoint). Data-leg
  import projection stays in the binders for now (deferred to 6b-2). build + vet + gofmt + full module suite green.
- **Refactor** (w8-block6b-2) Root import content create+bind → generic binder (content-single-writer design §10,
  creator=binder): the import root SnapshotContent is now created + bound by the GenericSnapshotBinderController
  exactly like the capture root, not by the namespace Snapshot orchestrator. reconcileGenericImport gained a root
  branch — when ResolveParentSnapshotContentOwnerRef yields no parent ownerRef (and !pending) and the object
  IsRootSnapshot, it anchors the content on the root ObjectKeeper (EnsureRootObjectKeeperWithTTL +
  RootObjectKeeperOwnerReference) and returns after create+bind+ownerRef-align (isRoot early-return) so the
  leaf-only tail (MCP-gate wait, DataImport data leg, Ready mirror) never runs on the structural root — avoiding a
  second writer on the root snapshot's Ready. snapshot/import.go reconcileImport is now content-free: it drops the
  MCP precondition + Create/bind and only holds ImportPending until boundSnapshotContentName is set by the binder,
  then mirrors the bound content's Ready (the aggregator projects manifest+children). The content name is
  identical either way (StableContentName and the old GenerateSnapshotContentName both resolve to
  names.ContentName(uid)), so no dual-content risk on migration. Removed now-dead helpers desiredImportSnapshotContentSpec
  + bindImportSnapshotContent (import.go) and finishReconcileWithExistingContent + snapshotContentObjectMeta +
  snapshotContentName + the already-dead desiredSnapshotContentSpec (controller.go); the reconcileImport rootOK
  param is dropped (the caller still ensures the root keeper for the Snapshot record's own TTL + root static-bind).
  build + vet + gofmt + snapshot/genericbinder unit suites green.
- **Refactor** (w8-block6b-3) Import data leg → aggregator single writer (content-single-writer design §10/§11.4):
  the SnapshotContentController is now the sole writer of status.data for import too, closing the last import
  writer split. Two paths: (B1) GENERIC import leaf — new reconcileDataLegProjection import branch
  (usecase.IsUnstructuredImportMode(owner) -> projectContentDataLegFromDataImport) gated on
  GVKRegistry.RequiresDataArtifact so structural nodes (root/VM) short-circuit; it reverse-looks-up the DataImport
  (FindDataImportForLeaf), builds the leaf-sourced binding (BuildImportDataBinding, moved from genericbinder into
  the aggregator package so both the aggregator publish and the binder terminal-precondition share one pure fn),
  then runs the same enrich + Retain/ownerRef VSC handoff + publish as the capture VCR branch. genericbinder/import.go
  drops projectDataLegFromDataImport (content write) — it keeps ONLY the two leaf-facing jobs the aggregator cannot
  do: surface the non-retryable artifact terminal on the leaf (via the moved BuildImportDataBinding) and mirror the
  aggregator-published content.status.data onto the leaf for d8 export (mirrorLeafDataFromContent, which already
  reads content.status.data); the now-dead `requeue` latch is removed (the existing !Ready poll drives the export
  mirror). (B2) native-CSI import VolumeSnapshot — volumesnapshotimport now publishes the recovered orphan PVC as
  status.snapshotSource (importSnapshotSourceRef + publishVolumeSnapshotSource) instead of writing the content, so
  the aggregator's existing native-CSI branch (projectContentDataLegFromBoundVSC / volumeSnapshotOwnerSource)
  builds {source PVC, bound VSC} and publishes — which also FIXES a latent bug: that branch requeued forever on an
  empty snapshotSource for imports (import VS never reached Ready post-Block-3, masked by compile-only night e2e).
  The controller still forces Retain, sets the CSI back-ref + legacy bound (readyToUse), recovers the orphan PVC,
  and mirrors the aggregator-published content.status.data onto the VS (polls until published). Removed importDataBinding
  (superseded by snapshotSource + the aggregator binding) + its content data writes; moved its pure PVC-source test
  to TestImportSnapshotSourceRef_TargetsPVC and BuildImportDataBinding tests to the aggregator package. New aggregator
  tests: import structural-node skip + full DataImport->VSC publish/handoff. build + vet + gofmt + full module suite green.
- **Add** (w8-block6 e2e) Import round-trip + MCP durability assertions (compile-check). Extended e2e/tests/import_gc_test.go
  importSpecs (export->import round-trip): after the import root reaches Ready + all content leg conditions, assert (a) the
  aggregator projected content.status.manifestCheckpointName (content-single-writer §10 single-writer, w8-block6b-1), and (b)
  the reconstructed ManifestCheckpoint is owned by the SnapshotContent — the durable end-state of the w8-block6a durability
  handoff (per-CR upload creates the checkpoint GC-safe under a dedicated import ObjectKeeper; the aggregator hands it off to
  the content and sweeps the redundant keeper), so deleting the content GCs the checkpoint. The existing round-trip already
  exercises the binder-created import root (w8-block6b-2). e2e/tests compiles (go test -c) + vet + gofmt clean; not run live
  (compile-check only per the night-run plan).
- **Design** (w8, plan-only, no code) Main-owned `commonController` decision (2026-07-08) — reverses the
  §8.1 "binder computes subtreePlanned" RESOLVED scoping. The aggregator (`SnapshotContentController` / main)
  becomes the sole writer of the whole `xxxSnapshot.status.captureState.commonController` sub-structure
  (`manifestCaptured`/`dataCaptured` latches, `subtreeManifestsPersisted` mirror, `subtreePlanned`), written
  **sideways** onto the snapshot (same path as the existing `Snapshot.Ready` mirror), and it performs the
  MCR/VCR **reap** in the same pass; the binder becomes a pure creator (writes nothing on any `status`). Key
  rationale captured: the leg latches are suppression markers read by the domain via an **authoritative
  uncached read** when it sees the request absent, so the latch MUST be set on the snapshot **before** the
  reap **by the reaper** — a "content field + binder mirror" scheme opens a suppression-timing window
  (domain re-creates a just-reaped request), so the field stays snapshot-native + main-written (not
  content-native + mirrored). Updated `docs/content-single-writer-design.md` (§2 diagram, §3 split, §8.1
  writer, §11.4 dataCaptured) and the ADR `2026-06-29-unified-snapshots-overview.md` (component roles,
  creator role, status model, subtreePlanned section, capture sequence diagram). Code (Block 7 subtreePlanned
  via main + the latch/reap consolidation, drop the vestigial binder MCR-watch) deferred to a deliberate,
  live-verified block per the direction change.
- **Refactor** (w8-block7 Part A) Main-owned `commonController` latch+reap moved off the binder onto the
  aggregator. The capture-leg lifecycle that lived in `genericbinder` (eager-init of the
  `commonController.manifestCaptured`/`dataCaptured` legs, the monotonic latches, the
  `subtreeManifestsPersisted` snapshot-mirror, and the MCR/VCR reap) now lives in
  `snapshotcontent/capture_legs.go` (`reconcileOwnerCaptureLegs`), wired into the `SnapshotContentController`
  reconcile loop and folded into the owner Ready mirror (`mirrorReadyToOwnerSnapshot` gained
  `legTermReason`/`legTermMessage` for the same-pass data-leg terminal). The latch is written sideways on the
  `xxxSnapshot` **before** the reap by the same actor, closing the suppression-timing window (decision #10).
  The binder is now a pure creator: dropped `ensureSnapshotContentLinks`/`ensureDomainContentLinks`,
  `eagerInitCaptureLegs`, `markCaptureLegCaptured`, `mirrorSubtreeManifestsPersistedFromContent`,
  `deleteManifest/VolumeCaptureRequest`, and the vestigial MCR watch (`mcr_watch.go`); it keeps only the leaf
  `status.data` export mirror. Tests: removed the now-moved binder tests (`domain_content_test.go` trimmed to
  the leaf-mirror + binding-map tests, `subtree_mirror_test.go` deleted) and ported their coverage to the new
  `snapshotcontent/capture_legs_test.go` (manifest/data latch+reap, native-CSI dataCaptured, pending-requeue,
  failed-terminal, subtree-mirror monotonicity, eager-init, writer-switch/barrier guards); updated the
  `mirrorReadyToOwnerSnapshot` call site for the new signature. build + vet + gofmt + full controller module
  suite green.
- **Bugfix** (w8-block7, tests) Deferred the stale `duplicate pvcUID in subtree fails closed with
  DuplicateCoveredPVCUID` integration spec (`n5_pr7_two_pvc_integration_test.go`) to `PIt`. Investigation
  (per direction: "if it's a real prod bug, fix prod until green") proved it is NOT a prod bug: the spec was
  red on clean HEAD too (verified by stashing all Block 7 work and re-running — identical failure at the same
  line), so Block 7 did not cause it. Root cause is the §8.5 data-bearing coverage gate:
  `coveredPVCUIDsForContent` keys coverage on `RequiresDataArtifact(kind)` (CSD authority), and the spec's
  synthetic children are core `Snapshot` (a built-in pair → not data-bearing), so their fixture `dataRef`s are
  never read and the guard cannot fire in envtest. This is a DELIBERATE, pinned invariant —
  `TestCollectSubtreeCoveredPVCUIDs_notDataBearingSkips` asserts a non-data-bearing child contributes nothing
  EVEN with `status.data` present — so altering prod would break the design, not fix a bug. The guard itself
  stays fully covered at the unit level by `TestCollectSubtreeCoveredPVCUIDs_duplicateUIDFailsClosed`
  (data-bearing children). The spec's own sibling (`pending VCR spec.targets…`) was already `PIt` for the
  identical "needs a registered data-bearing domain kind — out of scope for core envtest" reason; the dup-guard
  spec was left active by a block5 oversight. Fixed the now-false "stays active" comment and added the deferral
  rationale inline. Isolated pass green.
- **Bugfix** (w8-block7 Part A, review) Removed the duplicate root-side writer of
  `captureState.commonController.manifestCaptured`. Bugbot flagged that the namespace-root Snapshot is a
  domain-capture kind (main.go dogfooding), so main's `reconcileOwnerCaptureLegs` already eager-inits + latches
  + reaps the root's manifest leg, yet `SnapshotReconciler.stampRootManifestCaptured` (ready_patch.go, called
  from namespace_capture_run.go step 7) still wrote the same field — a decision #10 single-writer violation
  (Part A moved the binder's half to main but left the root reconciler's own stamp). The stamp could pre-latch
  `manifestCaptured=true` (off `content.subtreeManifestsPersisted`) before main reaped the root MCR, making main
  skip the reap (latch already true) and leak the transient MCR until the TTL sweeper. Deleted
  `stampRootManifestCaptured` + its call; main is now the sole latch/reaper (verified safe: `EnsureManifestCapture`
  always creates the root MCR and publishes `manifestCaptureRequestName`, so main always latches — even for an
  empty exclude set). Removed the three `TestStampRootManifestCaptured*` unit tests (behavior now covered by
  snapshotcontent/capture_legs_test.go), updated stale "binder/stamp latches" comments (namespace_capture_run.go,
  snapshot_adapter.go). build + vet + gofmt + full unit suite + non-isolated integration pass green.
- **Add** (w8-block7 e2e) Block 7 e2e coverage (compile-check only per the night-run grant). `aggregated_api_test.go`:
  a "Block 7" Context on the shared manifest-only tree with four specs — (1) `subtree-manifest-identities`
  serves a de-duplicated identity set spanning root + child subtree (design §8.3), (2) subtree-captured objects
  are excluded from the root own-manifests so nothing is double-captured, (3) main latches
  `commonController.manifestCaptured=true` on every `xxxSnapshot` node (never on its content) and reaps the MCR
  with no churn (decision #10), (4) `commonController.subtreePlanned=true` is latched snapshot-native on every
  node. `volumedata_test.go`: two env-gated specs — `dataCaptured` latch + VCR reap (no churn) on each domain
  data leaf (§11.4), and `subtreePlanned` bottom-up latching gating the orphan/residual wave so the root exclude
  is neither premature nor duplicated (§8.1/§8.4). Shared helpers in `e2e_shared_test.go`
  (`subManifestsIdentities`, `manifestCaptureRequestGVR`, `coreContentSubPath`, `snapshotCommonControllerLatch`);
  `rootManifestCaptured` now delegates to the shared latch reader. Review: three Bugbot passes — fixed two
  context-vs-`Eventually` budget mismatches (the `subtreePlanned` and `dataCaptured` volume specs cancelled
  before their inner waits) and made the root-exclude spec poll the fail-closed endpoint + own-manifests together
  (one-shot reads raced the still-converging tree); final Bugbot pass clean. gofmt + vet + `go test -c` green.
- **Add** (capture-namespace) The root namespace Snapshot now captures its own `Namespace` object.
  `BuildRootNamespaceManifestCaptureTargets` (root_capture_run_exclude.go) appends a `v1/Namespace` target named
  after the target namespace, so the root MCP always holds the Namespace (served verbatim via `/manifests`,
  dropped by the restore sanitizer as cluster-scoped) and the root MCR is never empty. The capture executor
  `collectTargetObjects` (checkpoint_controller.go) now resolves each target's scope via the RESTMapper and
  TERMINALLY rejects (terminalCaptureError → Ready=False/Failed, not requeue) any cluster-scoped target except the
  Namespace whose name equals `mcr.Namespace`, for which it does a cluster-scoped GET (captured with empty
  `metadata.namespace`); a RESTMapping miss stays transient (requeue). The MCR webhook `MCRValidate`
  (mcrValidator.go) now allows a `v1/Namespace` target only when its name equals the MCR namespace, authorized via a
  cluster-scoped `namespaces get` SubjectAccessReview. Unit tests: executor scope-guard specs (own Namespace
  accepted; foreign Namespace / other cluster-scoped terminally rejected; namespaced regression) + a Reconcile-level
  terminal-classification test (all fake clients now carry a RESTMapper); mcrValidator accept/reject specs; a
  root-target exclude-key invariant test. gofmt + vet + unit suites green (golangci-lint unavailable locally);
  Bugbot clean on the changed files.
- **Add** (capture-namespace e2e) `e2e/tests/namespace_manifest_capture_test.go` (`namespaceManifestCaptureSpecs`,
  registered in the shared "Phase 1 & 2" Context after `namespaceCaptureReworkSpecs`). One positive spec: a root
  Snapshot's own-manifests download (manifests-download subresource) now contains the namespace's own `Namespace`
  object, verbatim and cluster-scoped (`apiVersion=v1`, `metadata.namespace==""`). Three MCR-admission specs create
  MCRs directly via the cluster-admin `suiteDyn`: a non-Namespace cluster-scoped target (ClusterRole) is rejected
  ("cluster-scoped"), a foreign `Namespace` target (name != MCR namespace) is rejected ("own namespace"), and the
  MCR's own `Namespace` target (name == MCR namespace) is accepted (authorized via the cluster-scoped
  `namespaces get` SAR); the accepted MCR is explicitly deleted in `DeferCleanup` so no cluster-scoped remnant
  outlives namespace teardown. Added the local `manifestCaptureRequest` unstructured constructor. Review: explore
  subagent (helper/signature + sibling-convention + webhook-message consistency) clean, nits applied. make build +
  make vet green (full suite needs a cluster).
- **Update** (capture-namespace docs) Corrected the now-stale `ManifestCaptureRequest` API doc comments to reflect
  the Namespace exception: `ManifestCaptureRequestSpec.Targets` and `ManifestTarget.Kind`
  (api/v1alpha1/manifestcapturerequest_types.go) no longer say cluster-scoped targets are flatly disallowed — they
  now state the single exception (the capture's own `Namespace`, core v1, whose name equals the MCR namespace).
  Mirrored the wording in the Russian doc-ru CRD and regenerated the CRDs (`bash hack/generate_code.sh`); the only
  regenerated diff is the two field descriptions in `crds/state-snapshotter.deckhouse.io_manifestcapturerequests.yaml`
  (no schema/deepcopy churn). gofmt clean.
- **Cleanup** (conditions) Pruned dead Ready reasons after a full producer audit (every declared reason grepped
  for real writers across the repo incl. e2e/sdk/hooks/webhooks): dropped `NoCaptureTargets`, `CapturePlanDrift`
  (drift detection replaced by the PIT model), `ContentRefMismatch` and `VolumeCaptureTargetsFailed` (its
  failCapture producers dismantled with w8-block3/3d) from `TerminalReadyReasons` (api) and
  `ChildSnapshotTerminalReadyReasons` (usecase); removed the never-produced `ChildSnapshotMissing`,
  `ChildGraphPending` (folded into `ChildrenPending` from day one), `NamespaceCaptureIncomplete` (the
  unreadable-plan path fail-closed-requeues without writing Ready since w8-block3d) and the `Ready` True-reason
  (the canonical True reason is `Completed`) from pkg/snapshot. `ArtifactFailed` deliberately KEPT — documented
  Phase-2 forward declaration (artifact-degradation revalidation). Rewrote the stale exhaustive failCapture
  catalog comment in ready_patch.go to the live producers (ListFailed / DuplicateCoveredPVCUID /
  SourceContentNotFound / SnapshotContentMisbound / NamespaceNotFound; the phantom `SubtreeManifestFailed`
  entry removed) and the capture.go unreadable-plan comment; test fixtures migrated off removed reasons
  (`ReasonReady` -> `ReasonCompleted` x28, `CapturePlanDrift` -> `VolumeCaptureFailed`). The ADR
  «Conditions & Reasons» catalog synced the same day (terminal 13 -> 9; non-terminal −2 dead,
  +`ChildrenLinkPending` +`ContentMissing` which were produced but uncatalogued). Unit tests green across
  api/usecase/snapshot/genericbinder/snapshotcontent; integration + e2e compile-checked.
- **Refactor** (genericbinder, sdk-children-planned-freeze block 0) Deleted the dead
  `commonControllerLegCaptured` helper from `domain_content.go` — zero callers remained after the w8-block7
  move of the capture-leg lifecycle to the aggregator, and it held the package's last quoted
  `"commonController"` status-key literal. Added `pure_creator_guard_test.go`: a dependency-free grep-guard
  (os.ReadDir over the package dir) that fails if that quoted literal reappears in any non-test source,
  pinning the pure-creator invariant (decision #10 — the binder never touches main-owned
  `captureState.commonController`). gofmt clean; `go test ./internal/controllers/genericbinder/` green.
- **Add** (snapshotsdk, sdk-children-planned-freeze block A) `EnsureChildren` now enforces the planned-freeze
  contract: once a node declares barrier 1 (phase>=Planned, incl. the terminal Failed) the declared child set
  is frozen. A fail-closed pre-check (authoritative apiReader re-read, mirroring `EnsureVolumeCapture`) rejects
  any GROWTH of the declared set or change of the excluded set with the new typed `ErrChildrenSetFrozen`
  BEFORE `children.Reconcile` creates/adopts anything, so a rejected call has zero side effects; desired ⊆
  published with an unchanged excluded set stays an idempotent no-op / ownerRef repair. Added the pure
  `childrenSetFrozen` predicate (mirrors `phaseCanAdvance`) plus an in-closure TOCTOU belt that drops a racy
  frozen growth (`patch.Status` closures cannot surface errors). Closes the wedge hazard from the live Block 4
  immutable `childrenSnapshotContentRefs` CEL — a late edge would otherwise wedge the node at
  `ChildrenLinkPending`. Fixed the stale delete-free docs (`doc.go`, `internal/children` package + `Reconcile`
  doc, `EnsureChildren` method doc): a no-longer-desired child's ref STAYS published (union never removes) —
  only the OBJECT is ownerRef-GC'd — and the lead sentences now read additive/union, not reconcile-to-match.
  New unit tests (`capture_children_freeze_test.go`): growth pre-Planned OK; same-set post-Planned no-op ×5;
  growth post-Planned/Finished/Failed → `ErrChildrenSetFrozen` with no CR created; excluded-change
  post-Planned rejected; freeze-predicate table test; TOCTOU belt. gofmt + go vet + `go test ./...` green;
  bugbot clean.
- **Add** (snapshotcontent/snapshot/api, sdk-children-planned-freeze block E) Detect and report a vanished
  declared child on the owner Ready mirror. Two new canonical reasons in `api/storage` (aliased in core
  `pkg/snapshot`): `ChildSnapshotDeleted` (NON-terminal, recoverable) and `ChildSnapshotLost` (terminal, added
  to `TerminalReadyReasons`). Detection lives in MAIN (`snapshotcontent/lost_children.go`) as a fold in
  `mirrorReadyToOwnerSnapshot`, applied LAST so a terminal Lost overrides even a phase=Finished Ready=True while
  a non-terminal Deleted only downgrades a would-be Ready=True; the CONTENT Ready stays intact (only the
  namespaced user surface degrades). Runs only past barrier 1 and is a no-op on a domain phase=Failed or a
  deleting owner. Frozen-edge mode (childrenSnapshotContentRefs set): a missing child content → Lost; a
  surviving recycle-bin content whose parent CR was deleted → Deleted if the content is Ready (restorable via
  StaticBind, self-heals to Ready) else Lost. Declared-ref mode (edges not frozen yet, post-Planned): an absent
  child CR → Lost. Pre-Planned domain case (case 4) in `namespace_children_plan.go`
  (`detectLostDomainChildrenPrePlanned`): at AllPlanned a published domain child whose source vanished (left the
  desired set) AND whose CR was also deleted is surfaced as terminal `ChildSnapshotLost` via the existing
  pre-Planned terminal outcome (no dual-writer — gated off post-Planned); a live source self-heals, a still-present
  CR is not lost, orphan CSI VolumeSnapshot children are excluded. Unit tests: full detection matrix + mirror-fold
  precedence + restore-heals-to-Ready + the three pre-Planned planner cases. ADR «Судьба исчезнувших объявленных
  детей» + reason catalog already carry the semantics. gofmt + go vet + tests green; golangci-lint adds no new
  findings.
- **Refactor** (snapshot, sdk-children-planned-freeze block B) Namespace-root capture no longer re-plans
  membership after barrier 1. `reconcileNamespaceCapture` now gates its whole plan+enumerate+freeze block
  (steps 1-5: PublishSnapshotSource, planNamespaceChildren, EnsureChildren, the residual/orphan-PVC wave,
  MarkPlanned) behind `namespaceDomainPrePlanned` — past Planned the composition is frozen (ADR PIT cycle
  «если узел уже Planned — план заморожен, состав не пересчитывается») and the reconciler jumps straight to the
  post-bind legs (6-8: wait-for-bind, manifest-exclude MCR, ConfirmConsistent), which self-requeue and are
  driven by the child/content watches, so convergence survives the skip. This removes the old accidental
  re-plan self-heal: a declared child CR deleted after Planned is deliberately NOT recreated and instead
  surfaces as terminal `ChildSnapshotLost` via block E (`lost_children.go` /
  `detectLostDomainChildrenPrePlanned`) — one strict window-independent semantics (deleted-after-Planned =
  lost). New integration spec in `snapshot_capture_plan_drift_test.go` (children-axis mirror of the existing
  manifest-axis frozen-plan spec): a residual PVC added after the root is captured is never enumerated
  (childrenSnapshotRefs stays empty, no VolumeSnapshot child) and the root stays Ready=True/Completed; verified
  to fail without the gate (the orphan wave would flip Ready=False on the classless PVC). Also fixed a stale
  sibling lifecycle spec (separate commit): the empty-namespace root MCP now correctly holds the captured
  Namespace object, so the assertion moved from "empty archive" to "exactly the Namespace object". gofmt + go
  vet + integration specs green; golangci-lint adds no new findings; bugbot clean.
- **Add** (snapshot/snapshotsdk, sdk-children-planned-freeze block D) Restore observability of the fail-closed
  unreadable namespace manifest plan (regression from w8-block3d, which silently requeued when
  `BuildRootNamespaceManifestCaptureTargets` reported unreadable resource types — Forbidden / partial
  discovery — leaving the GVR list only in controller logs). New SDK capability `CaptureProgress.ReportProgress`
  writes ONLY `captureState.domainSpecificController.message`, preserving the phase and reason (non-terminal,
  never touches the core-owned Ready) — the domain-owned diagnostic channel per the ADR status model.
  `reconcileNamespaceManifestLeg` now publishes the capped unreadable-GVR list (first 10 + count, sorted) via
  `ReportProgress` AND emits a Warning Event `NamespacePlanUnreadable` on the root Snapshot; both fire only when
  the set CHANGES (idempotent gate on the published message) so the 500ms fail-closed requeue does not flood.
  The message is persisted before the Event so a failed status patch cannot spam Events, and a recovered plan
  clears its own stale diagnostic (sentinel-prefix match) before `EnsureManifestCapture`. The deleted
  `NamespaceCaptureIncomplete` Ready reason is NOT resurrected (writer discipline: creator pre-bind / main
  post-bind). Unit tests: message+Event carry the GVR list, Ready not written by the domain, phase preserved,
  idempotent on unchanged set, clear-on-recovery (own vs foreign message), and the cap/sort formatter. gofmt +
  go vet + tests green; golangci-lint adds no new findings; bugbot clean.
- **Update** (docs, sdk-children-planned-freeze block C) Sync docs/content-single-writer-design.md §3.4 with
  the enforced declared-set freeze. Recorded the second (upper) floor of the freeze alongside the existing
  content-set immutable CEL (lower floor): the SDK `EnsureChildren` fail-closed guard (`ErrChildrenSetFrozen`
  at phase>=Planned/Failed, before any child CR) and the namespace-domain re-plan skip
  (`namespaceDomainPrePlanned` gate), with the wedge-hazard rationale and a cross-ref to the ADR «Фриз
  объявленного набора — enforced (SDK + домен)». Corrected the stale ordering note (Option A CEL is already
  live on wave7, so the guard closes an already-open wedge window rather than landing before the CEL). Also
  tightened the ADR SDK bullet to state the excluded-set freeze + typed `ErrChildrenSetFrozen` (matches the
  implemented semantics). Docs only.
- **Bugfix** (unifiedbootstrap) Fixed stale test assertion in
  `TestDefaultGraphRegistryBuiltInPairs_hasNoDomainPairs` (`gvk_test.go`) still checking the pre-rename API
  group `storage.deckhouse.io`; the group was renamed to `state-snapshotter.deckhouse.io` in `61e2795`
  (`refactor(api): rename API group storage.deckhouse.io -> state-snapshotter.deckhouse.io`) but this one
  assertion was missed, so the core built-in Snapshot pair itself tripped the "no hardcoded domain pair"
  guard. Production code (`gvk.go`) was already correct; test-only fix.
- **Bugfix** (snapshot) Namespace child planner no longer expands a `PVC -> native CSI VolumeSnapshot` CSD
  mapping (the storage-foundation-volumesnapshot shape) into a domain child. `buildNamespaceChildSpec` only
  emits the unified `spec.sourceRef`, but a native `snapshot.storage.k8s.io/VolumeSnapshot` requires
  `spec.source.persistentVolumeClaimName`, so the planner was POSTing an invalid VolumeSnapshot
  (`spec.source: Required value`) every reconcile — the child never got conditions and the root Snapshot
  stalled with no `captureState` (e2e Phase 3b `resourceSelector over PVC volume data` timed out). PVC volume
  capture is owned end-to-end by the root's residual/orphan wave (`ensureOrphanVolumeSnapshotsPrePlanned` ->
  `ensureOrphanPVCVolumeSnapshots`), which builds the correct `spec.source` + resolves the VolumeSnapshotClass
  and honors `resourceSelector`. New `isNativeCSIVolumeSnapshotMapping` gate in `planParentOwnedChildGraphLayer`
  skips only the child-spec build (and coverage seeding) for such mappings while STILL recording a veto-labeled
  PVC in `excludedRefs` (same treatment as a vetoed domain source). Unit tests: native VS mapping is not
  expanded (layer + end-to-end plan stays AllPlanned with no children) and a vetoed PVC is still recorded in
  `excludedRefs`. gofmt + go vet + tests green; golangci-lint adds no new findings; bugbot clean.
