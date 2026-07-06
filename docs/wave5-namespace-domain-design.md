# wave5 — Root `Snapshot` as an in-process SDK namespace-domain (design)

Status: **DESIGN / not implemented.** This is the design for the deferred wave5 todos
`w5-content-creation` + `w5-namespace-domain-sdk` (they are one change — see §2). Implement
attended, behind the envtest integration gate (`snapshot_root_lifecycle` / `snapshot_recreate` /
`snapshot_n1_boundary`). Companion execution log: `.cursor/plans/wave5_notes.md`.

Absolute paths are relative to `images/state-snapshotter-controller/` unless noted.

---

## 1. Goal & non-goals

**Goal.** Make the root `Snapshot` reconciler an in-process instance of the SAME capture SDK
(`pkg/snapshotsdk`) that external/demo domains use ("dogfooding"), and make the generic binder
(`internal/controllers/genericbinder/`) the *single* creator of `SnapshotContent` for **all** kinds
including the root. After this change the root reconciler is:

- **content-free** — it never creates/binds `SnapshotContent`; the binder does;
- **SDK-driven** — it plans children (`EnsureChildren`), requests the namespace manifest capture
  (`EnsureManifestCapture`), publishes `status.snapshotSource` (kind=`Namespace`) via
  `PublishSnapshotSource`, and drives the barrier (`MarkPlanned` → wait → `ConfirmConsistent` /
  `Reject`);
- a **writer of only** `status.captureState.domainSpecificController`, `status.childrenSnapshotRefs`
  and `status.snapshotSource` — exactly the adapter write-discipline the SDK enforces
  (`pkg/snapshotsdk/adapter.go:23-63`).

**Non-goals.**
- No change to the external domain contract or to demo domains.
- No change to the `SnapshotContent` controller (`internal/controllers/snapshotcontent/`) — it keeps
  owning `Ready` and data-readiness.
- No change to `storage-foundation` VCR/DataImport wire (already done in `w5-field-rename`).
- Restore/export and d8 are out of scope (d8 is a separate deferred todo that *depends* on this one:
  it needs the root to write `status.snapshotSource=Namespace`).

---

## 2. Why `w5-content-creation` and `w5-namespace-domain-sdk` are one change

Verified wiring facts:

- `cmd/main.go` builds the binder's watch set via
  `unifiedbootstrap.FilterGenericSnapshotGVKPairs(...)`, which drops the dedicated kinds
  (`Snapshot`, `DemoVirtualDiskSnapshot`, `DemoVirtualMachineSnapshot`) — see
  `pkg/unifiedbootstrap/pairs.go` (`DedicatedSnapshotControllerKinds`, `FilterGenericSnapshotGVKPairs`).
  So **today the binder never watches the root `Snapshot`**; root `SnapshotContent` is created solely
  by `internal/controllers/snapshot/controller.go` (`Reconcile`, the `IsNotFound → Create` block).
- The binder's `IsRootSnapshot` branch (`genericbinder/controller.go`) is for hand-created *top-level
  domain* snapshots, **not** the namespace root.

If we fold root content creation into the binder *without* rewriting the reconciler, two controllers
(`snapshot/controller.go` and the binder) both reconcile `Snapshot` and contend over
`status.boundSnapshotContentName` and capture ordering: the root controller runs the children-graph and
capture **on the content it currently creates synchronously**; if the binder creates it asynchronously
the controller must first poll for existence, and the "who owns the bind field" question becomes racy.

Therefore content-creation moves to the binder **only as part of** turning the reconciler into a
content-free SDK domain. Treat them as a single, staged change (§7 gives a safe cutover order).

---

## 3. Current vs target (component responsibilities)

| Concern | Today (root, bespoke) | Target |
|---|---|---|
| `SnapshotContent` create/bind | `snapshot/controller.go` (`Create` + `Status().Update` of `boundSnapshotContentName`) | **binder only** (already the pattern for domains: `genericbinder/controller.go` create + `PatchUnstructuredBoundContentName`) |
| Children graph | bespoke `snapshot/parent_graph.go` | root domain builds `[]ChildSpec` → `sdk.EnsureChildren` |
| Namespace manifest | bespoke `snapshot/capture.go` `ensureManifestCaptureRequest` | `sdk.EnsureManifestCapture` + publish MCR name into `domainSpecificController.manifestCaptureRequestName` |
| `status.snapshotSource` (Namespace) | not written | `sdk.PublishSnapshotSource` |
| Data leg projection onto content | bespoke `snapshot/capture.go` + `volume_capture.go` | **binder** `ensureDomainContentLinks` (already does VCR→VSC handoff→`status.data` for domains) |
| Orphan/residual PVC wave | `snapshot/volume_capture.go` (creates CSI VS / child volume nodes) | folded into `EnsureChildren` as volume-child `ChildSpec`s (§6.2) |
| `boundSnapshotContentName` | dual writer (root + would-be binder) | **single writer: binder** |
| `commonController.manifestCaptured` (root RBAC latch) | `snapshot/ready_patch.go` `stampRootManifestCaptured` | stays core (binder `eagerInitCaptureLegs`/`markCaptureLegCaptured`); root RBAC carve-out retained (§6.4) |
| static-bind / import / delete-retain | `snapshot/{static_bind,import}.go` + `reconcileDelete` | binder's generic paths (`reconcileGenericStaticBind`/`reconcileGenericImport`) extended to the root (§6.5) |

**Shape match:** the root is an **aggregator** domain — namespace manifest + children, no single-PVC
data leg of its own — so it maps to the **DemoVirtualMachineSnapshot** pattern
(`images/domain-controller/internal/controllers/demo/virtualmachinesnapshot_controller.go`:
`EnsureChildren` + `EnsureManifestCapture` + `MarkPlanned` + wait `allChildrenCaptured` +
`ConfirmConsistent`), **not** the single-volume DemoVirtualDiskSnapshot pattern.

---

## 4. The SDK seam the root must implement

`pkg/snapshotsdk` exposes two seams:

- `SnapshotAdapter` (domain implements) — `pkg/snapshotsdk/adapter.go:23-63`:
  `Object`, `SourceRef`, `Get/SetDomainCaptureState`, `Get/SetSnapshotSource`, `CoreCaptureState`,
  `ReadyReason`, `ReadyMessage`. Writer discipline: the SDK writes ONLY
  `captureState.domainSpecificController`, `childrenSnapshotRefs`, `snapshotSource`; it only *reads*
  `commonController` and `Ready`.
- `CaptureSDK` (SDK provides) — `pkg/snapshotsdk/capture.go:34-98`:
  `EnsureChildren`, `EnsureVolumeCapture`, `EnsureManifestCapture`, `MarkPlanned`,
  `ConfirmConsistent`, `Fail`, `Reject`, `PublishSnapshotSource`; constructed with
  `snapshotsdk.New(client, apiReader, provider)`; read-only tri-state via
  `snapshotsdk.CoreCaptureOutcome(adapter)`.

### 4.1 New `NamespaceSnapshotAdapter`

Add a root adapter next to the demo one (mirror `images/domain-controller/internal/controllers/demo/snapshot_adapter.go`), but living in the core controller module (the root reconciler is in
`state-snapshotter-controller`, not `domain-controller`):

- `Object()` → the `*storagev1alpha1.Snapshot` (typed; root is a first-class API type, so the adapter
  can be typed rather than unstructured — simpler than the demo unstructured adapter).
- `SourceRef()` → `{Kind: "Namespace", Name: snapshot.Namespace}` (the namespace is the logical
  source of a root snapshot).
- `Get/SetDomainCaptureState()` ↔ `snapshot.Status.CaptureState.DomainSpecificController` +
  `snapshot.Status.ChildrenSnapshotRefs` (typed fields already exist —
  `api/storage/v1alpha1/capture_state_types.go`).
- `Get/SetSnapshotSource()` ↔ `snapshot.Status.SnapshotSource` (added in `w5-api`, `5308a73`).
- `CoreCaptureState()` → reads `snapshot.Status.CaptureState.CommonController`.
- `ReadyReason/Message()` → reads `snapshot.Status.Conditions[Ready]`.

The adapter must persist via `client.Status().Patch` with an optimistic-lock merge (the demo adapter
pattern), so it never clobbers the core-written `commonController`/`Ready`.

### 4.2 Root planning controller (in-process)

Replace the body of `snapshot/controller.go` `Reconcile` (capture path) with the aggregator recipe
(cf. `virtualmachinesnapshot_controller.go:150-199`):

```text
Reconcile(Snapshot):
  handle deletion / finalizer / TTL keeper           (unchanged core concerns, §6.5/§6.6)
  if import mode:      return binder-driven import    (§6.5)
  if static-bind mode: return binder-driven bind      (§6.5)

  adapter := NewNamespaceSnapshotAdapter(snap)
  sdk     := snapshotsdk.New(Client, APIReader, volumeProvider)

  sdk.PublishSnapshotSource(ctx, adapter, {Kind: Namespace, Name: snap.Namespace})

  desired, excluded := planNamespaceChildren(ctx, snap)   // resource graph + orphan/residual PVCs (§6.2)
  sdk.EnsureChildren(ctx, adapter, desired, excluded)

  sdk.EnsureManifestCapture(ctx, adapter, namespaceManifestSpec(snap))   // §6.3

  sdk.MarkPlanned(ctx, adapter)                            // phase → Planned; unblocks binder

  switch snapshotsdk.CoreCaptureOutcome(adapter) {         // reads commonController latches
    Pending:   requeue (also wait allChildrenCaptured, cf. VM:203-230)
    Succeeded: sdk.ConfirmConsistent(ctx, adapter)         // phase → Finished
    Failed:    sdk.Reject(ctx, adapter, ...)               // phase → Failed
  }
```

Everything the reconciler does today AFTER planning (create content, bind, publish content children,
run MCR→MCP, VCR→VSC handoff, mirror `Ready`) moves to the **binder** for the root, exactly as it
already runs for domains.

---

## 5. Binder becomes the sole content creator (incl. root)

The binder already implements the full content lifecycle for domain kinds
(`genericbinder/controller.go` `Reconcile`: barrier `phase>=Planned` → `eagerInitCaptureLegs` →
create content / bind → `ensureSnapshotContentLinks` → `checkConsistencyAndSetReady`;
`genericbinder/domain_content.go` `ensureDomainContentLinks`: children projection + VCR data-leg
handoff + latch stamping). The root must flow through the **same** code:

1. **Wire the root into the binder watch set.** Move `"Snapshot"` out of
   `DedicatedSnapshotControllerKinds` and into `DomainCaptureSnapshotKinds`
   (`pkg/unifiedbootstrap/pairs.go`), OR add an explicit root pair to the binder's watch list and mark
   it `MarkDomainCaptureKind`. Result: `FilterGenericSnapshotGVKPairs` no longer removes the root, and
   `unifiedruntime/syncer.go` marks the root as a domain-capture kind (binder owns its content).
2. **Root ObjectKeeper.** The binder's `IsRootSnapshot` branch (`controller.go`) already ensures the
   root `RootObjectKeeperOwnerReference` and uses it as content owner — this is exactly what the root
   needs (it is a root). Confirm it triggers for `Snapshot` once watched (it keys on "no parent owner
   ref", which the namespace root satisfies).
3. **`boundSnapshotContentName` single-writer.** After cutover, delete the root's direct
   `Status().Update` of `boundSnapshotContentName` (`snapshot/controller.go`); the binder's
   `PatchUnstructuredBoundContentName` becomes the only writer. The root reconciler simply *reads* the
   field to know its content exists.
4. **Data-leg projection.** Root child volume leaves are ordinary domain leaves to the binder; their
   VCR→VSC→`status.data` handoff is already `ensureDomainContentLinks`. The root's own content has no
   single data binding (aggregator) — it only aggregates `childrenSnapshotContentRefs` (already done by
   `PublishSnapshotContentChildrenFromSnapshotRefs`).

---

## 6. Root-specific carve-outs (what does NOT fit the demo shape)

These are the reasons the root was bespoke; each needs an explicit decision.

### 6.1 Namespace source (not a PVC)
The root's `SourceRef` is `Namespace`, not a PVC. `EnsureVolumeCapture` (single-PVC data leg) is
**not** called for the root. Confirm `PublishSnapshotSource` accepts a `Namespace` kind (the field and
enum were added in `w5-api`).

### 6.2 Children planning = resource graph + orphan/residual PVC wave
`planNamespaceChildren` must reproduce what `parent_graph.go` + `volume_capture.go` do today, as
`[]ChildSpec`:
- CSD-eligible resource mappings, weight layers, `resourceSelector` (`parent_graph.go:45-135`).
- Exclude-label veto (`snapshotsdk.PartitionExcluded`) — the SDK already publishes
  `domainSpecificController.excludedRefs`, replacing the root's manual
  `publishSnapshotTopLevelExcludedRefs` (`parent_graph.go:137-176`).
- **Orphan/residual PVCs** (PVCs not owned by a planned workload) become **volume-child ChildSpecs**
  (generic PVC leaves), instead of the bespoke CSI-VolumeSnapshot wave in `volume_capture.go`. Each
  becomes its own `SnapshotContent` (Variant-A cardinality ≤1), bound & data-captured by the binder.
  *This is the largest behavioral consolidation and the main risk area — gate hard.*

Weight-layer ordering (`weightLayerCaptureReady`, `parent_graph.go:459-495`) maps onto the aggregator
barrier: `MarkPlanned` only after all layers are planned; `ConfirmConsistent` only after
`allChildrenCaptured` (cf. VM `childCoreCaptureState`/`AllLegsCaptured`).

### 6.3 Namespace manifest capture (one MCR for the whole namespace)
`EnsureManifestCapture` today (demo) creates a per-object MCR. The root needs a **namespace** MCR
(`namespacemanifest.SnapshotMCRName(uid)`, targets from
`BuildRootNamespaceManifestCaptureTargets`, `capture.go:181-210`). Two options:
- (a) pass a `ManifestCaptureSpec` that already carries the namespace target set (preferred — keeps the
  SDK generic; the root builds the spec);
- (b) add a namespace variant to the provider. Prefer (a). The SDK then owns MCR create + publishes
  `domainSpecificController.manifestCaptureRequestName`; the binder chases MCR→MCP as it already does
  in `ensureSnapshotContentLinks`.

### 6.4 Root RBAC "manifest captured" latch
`stampRootManifestCaptured` (`ready_patch.go:89-127`) is a root-only carve-out (RBAC hook). Keep it
core-owned; it writes `commonController.manifestCaptured`, which the SDK *reads* (suppression) — no
conflict with the adapter write-discipline. Document it as the one core write into `commonController`
on the root path (the binder's `eagerInitCaptureLegs` initializes the latch to `false`).

### 6.5 static-bind / import
- Static-bind: fold `snapshot/static_bind.go` into the binder's `reconcileGenericStaticBind`
  (it already validates `spec.source.snapshotContentName ↔ content.spec.snapshotRef`, binds, mirrors
  `Ready`, recreates the child subtree). The root reconciler short-circuits before the SDK recipe when
  in static-bind mode.
- Import: the binder skips *root* import today (`genericbinder/import.go`), delegating to
  `snapshot/import.go`. Extend `reconcileGenericImport` to handle the root (create content
  `deletionPolicy=Delete`, publish MCP + child graph, mirror `Ready`) OR keep root import in
  `snapshot/import.go` but make it call the binder's content materializer. Prefer moving it to the
  binder so **all** content creation is in one place (the stated goal). Note d8 import consumption is a
  separate deferred todo.

### 6.6 delete / retain / TTL keeper
`reconcileDelete` (`controller.go:367-427`) + `EnsureRootObjectKeeperWithTTL` stay in the root
reconciler (lifecycle/finalizer concerns, not capture). The binder already handles content deletion +
finalizer removal on its side (`controller.go` Step-0). Ensure the two agree on
`deletionPolicy=Delete` vs `Retain` and that only one removes the content finalizer.

---

## 7. Cutover strategy (avoid dual-writer window)

The danger is a window where both `snapshot/controller.go` and the binder create/bind content. Stage:

1. **PR-A (no behavior change): extract & share.** Land the `NamespaceSnapshotAdapter` and
   `planNamespaceChildren` (pure planners) behind the existing bespoke path; add the binder root-watch
   *disabled* by a flag. Unit-test the adapter + planner in isolation. Green on the integration gate.
2. **PR-B (flip creation to the binder).** In one commit: (a) enable the binder to watch `Snapshot`
   (`DomainCaptureSnapshotKinds`), (b) delete the root's content `Create` + `boundSnapshotContentName`
   `Status().Update`, (c) switch the reconciler to the SDK recipe (`§4.2`). Because creation is now
   binder-only from the same commit, there is no dual-writer window. Keep static-bind/import bespoke in
   this PR (still root-owned) to shrink blast radius.
3. **PR-C: fold orphan/residual PVC wave into `EnsureChildren`.** Highest-risk consolidation; separate
   PR so it can be reverted independently. Gate on `snapshot_n1_boundary` + the two-PVC subtree spec.
4. **PR-D: move static-bind + import into the binder.** Finalize "single content creator".

Each PR must be green on the envtest integration gate before the next.

---

## 8. Single-writer invariants (must hold after every PR)

- `captureState.domainSpecificController.*`, `childrenSnapshotRefs`, `snapshotSource` — **root SDK
  adapter only**.
- `captureState.commonController.*` — **core only** (binder `eagerInitCaptureLegs`/`markCaptureLegCaptured`;
  root RBAC `stampRootManifestCaptured`). SDK reads, never writes (`adapter.go:23-63`).
- `boundSnapshotContentName` — **binder only** (after PR-B).
- `conditions[Ready]` — **core** (binder mirrors content `Ready`; root local planning failures via
  `sdk.Reject`/`Fail`, which write `domainSpecificController.phase=Failed`, bubbled by the binder —
  `genericbinder/controller.go` `domainCaptureFailed`).

Grep anchors for the co-write / optimistic-merge discipline to preserve: `single-writer`, `co-write`,
`carve-out`, `commonController`, `domainSpecificController` (SDK + binder + `snapshot/`).

---

## 9. Risks & test gates

- **Dual-writer regression** on `boundSnapshotContentName` — mitigated by PR-B atomicity (§7). Gate:
  `snapshot_root_lifecycle`.
- **Capture ordering** (binder creates content async; reconciler must poll for existence before
  publishing children) — the SDK barrier already sequences this (`MarkPlanned` gates the binder). Gate:
  `snapshot_recreate`.
- **Orphan/residual PVC** semantics change (bespoke CSI-VS wave → child volume nodes) — PR-C isolated;
  gate: two-PVC subtree spec (`n5_pr7_two_pvc_integration_test.go`) **once** the envtest
  `VolumeSnapshotContent` CRD gap is fixed (currently a pre-existing envtest limitation — see
  `wave5_notes.md` "Open risks"; the `isolated` spec times out without that CRD, unrelated to wave5).
- **Namespace MCR vs per-object MCR** — keep the namespace target builder; unit-test
  `namespaceManifestSpec`.
- **RBAC latch** — keep `stampRootManifestCaptured`; regression = root never reaches
  `manifestCaptured=true` → binder never publishes MCP.

New unit coverage to add alongside implementation: `NamespaceSnapshotAdapter` write-discipline
(only domain half + source + children), `planNamespaceChildren` (weight layers + exclude veto +
orphan PVCs → ChildSpec), `namespaceManifestSpec` targets. Integration: the three gate suites above.

---

## 10. Touch-list (files)

- `internal/controllers/snapshot/controller.go` — replace capture path with the SDK recipe; drop
  content `Create` + `boundSnapshotContentName` write.
- `internal/controllers/snapshot/snapshot_adapter.go` — **new** `NamespaceSnapshotAdapter`.
- `internal/controllers/snapshot/parent_graph.go` + `volume_capture.go` → **new** `planNamespaceChildren`
  (reuse the mapping/selector/weight logic; emit `[]ChildSpec`); retire the bespoke content-coupled bits.
- `internal/controllers/snapshot/capture.go` → `namespaceManifestSpec` (targets) fed to
  `EnsureManifestCapture`; retire bespoke MCR create/drive (binder owns MCP chase).
- `internal/controllers/genericbinder/controller.go` — confirm `IsRootSnapshot` + root content
  create/bind path covers `Snapshot`; ensure it is the only `boundSnapshotContentName` writer.
- `internal/controllers/genericbinder/import.go` — extend to root import (PR-D).
- `pkg/unifiedbootstrap/pairs.go` — move `"Snapshot"` to `DomainCaptureSnapshotKinds`.
- `pkg/unifiedruntime/syncer.go` — root now `MarkDomainCaptureKind` + binder watch.
- `cmd/main.go` — the root planning controller still registers, but now content-free; binder watch set
  includes the root.
- Tests as in §9.

---

## 11. Open questions to confirm before PR-B

1. `PublishSnapshotSource` + `snapshotSource` enum accept `Namespace` end-to-end (API + any consumer)?
2. Does `EnsureManifestCapture`'s `ManifestCaptureSpec` allow a namespace target set (option 7/6.3-a)
   without SDK changes, or is a small SDK addition needed?
3. Root import: move to binder (preferred) vs keep in `snapshot/import.go` calling the binder
   materializer — decide before PR-D (affects d8's namespaced import consumption).
