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
