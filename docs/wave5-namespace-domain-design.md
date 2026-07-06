# wave5 — Root `Snapshot` as an in-process SDK namespace-domain (design)

Status: **DESIGN / not implemented.** This is the design for the deferred wave5 todos
`w5-content-creation` + `w5-namespace-domain-sdk` (they are one change — see §2). Implement
attended, behind the envtest integration gate (`snapshot_root_lifecycle` / `snapshot_recreate` /
`snapshot_n1_boundary`). Companion execution log: `.cursor/plans/wave5_notes.md`.

Paths are relative to `images/state-snapshotter-controller/` **except**: `pkg/snapshotsdk/*` is the
repo-root shared SDK module (`repos/state-snapshotter/pkg/snapshotsdk/`, imported by both core and
external domain controllers — that is what makes the root dogfoodable), and demo-controller paths
(`images/domain-controller/…`) are given in full.

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
- `IsRootSnapshot` is by ownerRef (no snapshot parent), so it is a general *top-level* case that the
  namespace root **does** satisfy — it is NOT root-excluding. But today the binder's `IsRootSnapshot`
  branch (`genericbinder/controller.go`) never fires *for the root*, because the binder does not watch
  `Snapshot` (above); today it is reached only for hand-created top-level *domain* snapshots. Once the
  root is watched (§5.1) the same branch fires for it.

If we fold root content creation into the binder *without* rewriting the reconciler, two controllers
(`snapshot/controller.go` and the binder) both reconcile `Snapshot` and contend over
`status.boundSnapshotContentName` and capture ordering: the root controller runs the children-graph and
capture **on the content it currently creates synchronously**; if the binder creates it asynchronously
the controller must first poll for existence, and the "who owns the bind field" question becomes racy.

Therefore content-creation moves to the binder **only as part of** turning the reconciler into a
content-free SDK domain. Treat them as a single, staged change (§7 gives a safe cutover order).

> **Plan note.** The plan lists `w5-content-creation` and `w5-namespace-domain-sdk` as two sequential
> todos (content-creation first). That split assumed `w5-content-creation`'s open verification item —
> *«биндер уже вызывается для корневого Snapshot»* — is true. It is **not**: the root sits in
> `DedicatedSnapshotControllerKinds`, so `FilterGenericSnapshotGVKPairs` drops it and the binder does
> not watch it today (§5.1). Hence the two todos cannot land independently; execute them as the single
> staged change in §7.

---

## 3. Current vs target (component responsibilities)

| Concern | Today (root, bespoke) | Target |
|---|---|---|
| `SnapshotContent` create/bind | `snapshot/controller.go` (`Create` + `Status().Update` of `boundSnapshotContentName`) | **binder only** (already the pattern for domains: `genericbinder/controller.go` create + `PatchUnstructuredBoundContentName`) |
| Children graph | bespoke `snapshot/parent_graph.go` | root domain builds `[]ChildSpec` → `sdk.EnsureChildren` |
| Namespace manifest | bespoke `snapshot/capture.go` `ensureManifestCaptureRequest` | `sdk.EnsureManifestCapture` + publish MCR name into `domainSpecificController.manifestCaptureRequestName` |
| `status.snapshotSource` (Namespace) | not written | `sdk.PublishSnapshotSource` |
| Data leg projection onto content | bespoke `snapshot/capture.go` | root's own content = aggregator, **no data leg**; domain-child disk leaves use **binder** `ensureDomainContentLinks` (VCR→VSC→`status.data`). Orphan volume legs are the CSI path, separate row below |
| Orphan/residual PVC wave | `snapshot/volume_capture.go` + `orphan_pvc_volume_snapshot.go` (CSI VS + per-PVC MCR + leaf content + `childrenSnapshotRefs` ref) | **preserved**; emitted through `EnsureChildren` as `VolumeSnapshot` children (CSI path, no VCR — INV-ORPHAN1) (§6.2) |
| `boundSnapshotContentName` | dual writer (root + would-be binder) | **single writer: binder** |
| `commonController.manifestCaptured` (root RBAC latch) | `snapshot/ready_patch.go` `stampRootManifestCaptured` | stays core (binder `eagerInitCaptureLegs`/`markCaptureLegCaptured`); root RBAC carve-out retained (§6.4) |
| static-bind / import / delete-retain | `snapshot/{static_bind,import}.go` + `reconcileDelete` | binder's generic paths (`reconcileGenericStaticBind`/`reconcileGenericImport`) extended to the root (§6.5) |

**Shape match:** the root is an **aggregator** domain — namespace manifest + children, no single-PVC
data leg of its own — so it maps to the **DemoVirtualMachineSnapshot** pattern
(`images/domain-controller/internal/controllers/demo/virtualmachinesnapshot_controller.go`:
`EnsureChildren` + `EnsureManifestCapture` + `MarkPlanned` + `ConfirmConsistent`), **not** the
single-volume DemoVirtualDiskSnapshot pattern. **One deliberate divergence from the VM aggregator:** the
VM waits `allChildrenCaptured` + unfreeze/verify before `ConfirmConsistent` (crash-consistent group);
the namespace domain has **no consistency action**, so its `phase=Finished` fires right after planning
(orphan wave `Complete` + direct domain children `phase>=Planned`), without the `allChildrenCaptured`
wait — see §4.2/§6.2.

---

## 4. The SDK seam the root must implement

`pkg/snapshotsdk` exposes two seams:

- `SnapshotAdapter` (domain implements) — `pkg/snapshotsdk/adapter.go:23-63`:
  `Object`, `SourceRef`, `Get/SetDomainCaptureState`, `Get/SetSnapshotSource`, `CoreCaptureState`,
  `ReadyReason`, `ReadyMessage`. Writer discipline: the SDK writes ONLY
  `captureState.domainSpecificController`, `childrenSnapshotRefs`, `snapshotSource`; it only *reads*
  `commonController` and `Ready`.
- `CaptureSDK` (SDK provides) — `pkg/snapshotsdk/capture.go` (interface + methods):
  `EnsureChildren`, `EnsureVolumeCapture`, `EnsureManifestCapture`, `MarkPlanned`,
  `ConfirmConsistent`, `Fail`, `Reject`, `PublishSnapshotSource` (`capture.go:271`); constructed with
  `snapshotsdk.New(client, apiReader, provider)` (`capture.go:116`); read-only tri-state via
  `snapshotsdk.CoreCaptureOutcome(adapter)`.

### 4.1 New `NamespaceSnapshotAdapter`

Add a root adapter next to the demo one (mirror `images/domain-controller/internal/controllers/demo/snapshot_adapter.go`), but living in the core controller module (the root reconciler is in
`state-snapshotter-controller`, not `domain-controller`):

- `Object()` → the `*storagev1alpha1.Snapshot` (typed; root is a first-class API type, so the adapter
  can be typed rather than unstructured — simpler than the demo unstructured adapter).
- `SourceRef()` → `{Kind: "Namespace", Name: snapshot.Namespace}` (the adapter's lightweight identity
  ref; note this is the `SourceRef` type, distinct from the published `SnapshotSource` below — and it is
  unused on the root anyway, since `EnsureVolumeCapture` is never called for it, §6.1).
- `Get/SetDomainCaptureState()` ↔ `snapshot.Status.CaptureState.DomainSpecificController` +
  `snapshot.Status.ChildrenSnapshotRefs` (typed fields already exist —
  `api/storage/v1alpha1/capture_state_types.go`).
- `Get/SetSnapshotSource()` ↔ `snapshot.Status.SnapshotSource` (`SnapshotSourceObjectRef`, added in
  `w5-api`, `5308a73`). The published ref must be the **full** self-contained ref
  `{apiVersion: v1, kind: Namespace, name: <ns>, uid: <ns UID>}` (ADR root example; plan `{v1,Namespace,ns,uid}`),
  **not** just kind+name — so the reconciler must `GET` the `Namespace` to resolve its `UID`
  (`apiVersion=v1` is constant). `PublishSnapshotSource` early-returns on an all-empty ref, so the UID
  resolution must happen before it is called.
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

  nsUID := getNamespaceUID(ctx, snap.Namespace)          // GET Namespace — snapshotSource needs the UID
  sdk.PublishSnapshotSource(ctx, adapter,
      {APIVersion: "v1", Kind: "Namespace", Name: snap.Namespace, UID: nsUID})   // full self-contained ref

  desired, excluded := planNamespaceChildren(ctx, snap)   // resource graph + orphan/residual PVCs (§6.2)
  sdk.EnsureChildren(ctx, adapter, desired, excluded)

  sdk.EnsureManifestCapture(ctx, adapter, namespaceManifestSpec(snap))   // §6.3 — request namespace MCR

  sdk.MarkPlanned(ctx, adapter)                            // phase → Planned; unblocks binder

  // A namespace domain has NO consistency action (no freeze/unfreeze), so it Finishes as soon as
  // PLANNING is done — it does NOT wait for children dataCaptured / MCP execution / Ready (that is the
  // separate Ready gate, §5 / §8). Finish gate (ADR "Корень как встроенный namespace-домен", root
  // example `phase: Finished` note):
  //   (a) orphan wave latched Complete   — residualVolumeCapture.phase=Complete, AND
  //   (b) every DIRECT DOMAIN child reached domainSpecificController.phase>=Planned.
  // orphan VolumeSnapshot children have NO phase (no domain controller — §6.2); they gate via the wave
  // latch, not a child phase. So CoreCaptureOutcome (commonController latches) is NOT the phase gate.
  switch {
    planning error:                       sdk.Fail(ctx, adapter, ...)          // phase → Failed + reason
    !(orphanWaveComplete(snap) &&
      directDomainChildrenPlanned(snap)): requeue                              // still planning
    default:                              sdk.ConfirmConsistent(ctx, adapter)  // phase → Finished (no verify)
  }
```

`ConfirmConsistent` here is a phase-only transition (no verify step) — the namespace domain has nothing
to reconcile for consistency, unlike the VM aggregator whose `ConfirmConsistent` runs after
`allChildrenCaptured` + unfreeze/verify.

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

1. **Wire the root into the binder watch set.** **Add** `"Snapshot"` to `DomainCaptureSnapshotKinds`
   while **keeping** it in `DedicatedSnapshotControllerKinds` (`pkg/unifiedbootstrap/pairs.go`) — exactly
   as the two demo kinds are in **both** sets. `DomainCaptureSnapshotKinds` is a *strict subset* of
   `DedicatedSnapshotControllerKinds` (the dedicated planning controller stays activated **and** the
   binder additionally watches), so this is an **add, not a move**: removing the root from
   `DedicatedSnapshotControllerKinds` would deactivate its dedicated planning controller (the
   content-free SDK reconciler) and is wrong. The binder watch is wired **not** via
   `FilterGenericSnapshotGVKPairs` (the root stays dedicated, so that filter still drops it from the
   *generic* pair set — correct) but via the syncer's dedicated loop: once `"Snapshot"` is a
   domain-capture kind, `unifiedruntime/syncer.go` stops taking the fully-dedicated short-circuit
   (`syncer.go:154-162`) and instead calls `s.snap.MarkDomainCaptureKind(snapGVK)` + `AddWatchForPair`
   (`syncer.go:181-183`) — the binder now owns the root's content.
   *Ordering caveat:* in the single in-process manager that runs **both** the root planning controller
   and the binder, if the root reconciler registers a typed informer + field index, the activator gate
   (`syncer.go:176-180`) requires the planning controller to activate before the binder watch, to avoid
   an indexer conflict on the shared informer (same rule that orders demo children before parents).
2. **Root ObjectKeeper.** The binder's `IsRootSnapshot` branch (`controller.go`) already ensures the
   root `RootObjectKeeperOwnerReference` and uses it as content owner — this is exactly what the root
   needs (it is a root). Confirm it triggers for `Snapshot` once watched (it keys on "no parent owner
   ref", which the namespace root satisfies).
3. **`boundSnapshotContentName` single-writer.** After cutover, delete the root's direct
   `Status().Update` of `boundSnapshotContentName` (`snapshot/controller.go`); the binder's
   `PatchUnstructuredBoundContentName` becomes the only writer. The root reconciler simply *reads* the
   field to know its content exists.
4. **Data-leg projection.** The root's own content has no single data binding (aggregator) — it only
   aggregates `childrenSnapshotContentRefs` (already done by
   `PublishSnapshotContentChildrenFromSnapshotRefs`). The root's DIRECT volume children are
   **orphan/standalone PVCs**, which take the **CSI `VolumeSnapshot` path, NOT the VCR path**: `VCR` is
   forbidden for orphan PVCs (ADR INV-ORPHAN1); their durable artifact is the bound VSC
   (`deletionPolicy=Retain`, INV-ORPHAN2), and their leaf content is created **typed** via
   `snapshotcontent.EnsureVolumeChildContent` (bypasses `getSnapshotContentGVK` and the generic VCR
   handoff). Domain-subtree disk leaves (e.g. VM→disk) DO use the binder's
   VCR→VSC→`status.data` handoff (`ensureDomainContentLinks`), but those are owned by the domain child,
   not by the root.

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
- **Orphan/residual PVCs** (PVCs not owned by a planned workload) are emitted by `EnsureChildren` as
  **`VolumeSnapshot` children** (kind=`VolumeSnapshot`) — the existing orphan model, **preserved, not
  rewritten** (do NOT turn them into generic VCR-captured PVC leaves). Each orphan PVC already gets its
  own CSI `VolumeSnapshot` + per-PVC MCR + leaf content (typed `EnsureVolumeChildContent`, CSI path,
  **no VCR** — INV-ORPHAN1) + durable bound VSC (`deletionPolicy=Retain`, INV-ORPHAN2), and its ref is
  already added to `root.status.childrenSnapshotRefs` (Variant A) by
  `reconcileOrphanPVCVolumeSnapshotChildLeaves` (`orphan_pvc_volume_snapshot.go`). The
  `residualVolumeCapture` wave-completion latch (`residualVolumeCapture.phase=Complete`) is preserved —
  it gates the first `Ready=True` **and** (per §4.2/§6.2 barrier) the root's `phase=Finished`. The wave5
  change here is **only the emission seam**: the SDK's `EnsureChildren` owns the child list uniformly,
  while the CSI capture path, per-PVC MCR, invariants, and latch are unchanged. *Re-routing emission is
  the main risk area — gate hard; behavior must be byte-for-byte the pre-wave5 orphan wave.*

Weight-layer ordering (`weightLayerCaptureReady`, `parent_graph.go:459-495`) maps onto **barrier-1
only**: `MarkPlanned` fires after all layers are planned. Unlike the VM aggregator, the namespace
domain has **no consistency action**, so `ConfirmConsistent` (→ `phase=Finished`) fires as soon as the
orphan wave is latched `Complete` (`residualVolumeCapture.phase=Complete`) **and** every direct DOMAIN
child reached `phase>=Planned` — it does **not** wait for `allChildrenCaptured` / children `Ready` (ADR
root example `phase: Finished` note: *«у namespace-домена нет действий согласованности → Finished сразу
после orphan-волны и phase>=Planned прямых детей»*). Full-subtree capture/readiness is the separate
`Ready` gate (binder + `SnapshotContentController`), which `phase=Finished` unblocks but never waits on.

### 6.3 Namespace manifest capture (one MCR for the whole namespace)
`EnsureManifestCapture` today (demo) creates a per-object MCR. The root needs a **namespace** MCR
(`namespacemanifest.SnapshotMCRName(uid)`, targets from `BuildRootNamespaceManifestCaptureTargets`,
`internal/usecase/root_capture_run_exclude.go:67` — the wave-barrier exclude-set builder, *called* from
`capture.go`, not defined there). Two options:
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
2. **PR-B (flip creation to the binder).** In one commit: (a) add `Snapshot` to
   `DomainCaptureSnapshotKinds` (keep it in `DedicatedSnapshotControllerKinds`) so the binder watches it
   (§5.1), (b) delete the root's content `Create` + `boundSnapshotContentName`
   `Status().Update`, (c) switch the reconciler to the SDK recipe (`§4.2`). Because creation is now
   binder-only from the same commit, there is no dual-writer window. Keep static-bind/import bespoke in
   this PR (still root-owned) to shrink blast radius.
3. **PR-C: route the (existing) orphan `VolumeSnapshot` wave through `EnsureChildren`.** Emission seam
   only — the CSI capture path, per-PVC MCR, invariants (INV-ORPHAN1/2), the `childrenSnapshotRefs` ref
   (Variant A) and the `residualVolumeCapture` latch are all **preserved**. Separate PR so it can be
   reverted independently. Gate on `snapshot_n1_boundary` + the two-PVC subtree spec.
4. **PR-D: move static-bind + import into the binder.** Finalize "single content creator".

Each PR must be green on the envtest integration gate before the next.

---

## 8. Single-writer invariants (must hold after every PR)

- `captureState.domainSpecificController.*`, `childrenSnapshotRefs`, `snapshotSource` — **root SDK
  adapter only** (the in-process namespace domain, via the adapter's `SetDomainCaptureState`). NB: the
  ADR root example annotates `childrenSnapshotRefs` *"← ядро"* — read that as "the in-process namespace
  domain, which runs in the core process", **not** the kind-agnostic core services; it is the same
  DOMAIN writer that domain nodes use for their `childrenSnapshotRefs`. Orphan `VolumeSnapshot` child
  refs (today emitted by the core orphan path `reconcileOrphanPVCVolumeSnapshotChildLeaves`) move under
  this same domain emission via `EnsureChildren` (§6.2), so the field stays single-writer.
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
- **Orphan/residual PVC** emission moves to `EnsureChildren`; the CSI-VS wave + per-PVC MCR +
  invariants (INV-ORPHAN1/2) + `residualVolumeCapture` latch are **unchanged** (only the emission seam
  moves) — PR-C isolated; gate: two-PVC subtree spec (`n5_pr7_two_pvc_integration_test.go`) **once** the
  envtest `VolumeSnapshotContent` CRD gap is fixed (currently a pre-existing envtest limitation — see
  `wave5_notes.md` "Open risks"; the `isolated` spec times out without that CRD, unrelated to wave5).
- **Namespace MCR vs per-object MCR** — keep the namespace target builder; unit-test
  `namespaceManifestSpec`.
- **RBAC latch** — keep `stampRootManifestCaptured`; regression = root never reaches
  `manifestCaptured=true` → binder never publishes MCP.

New unit coverage to add alongside implementation: `NamespaceSnapshotAdapter` write-discipline
(only domain half + source + children), `planNamespaceChildren` (weight layers + exclude veto +
orphan PVCs → `VolumeSnapshot` children, CSI path, no VCR), the `phase=Finished` gate (orphan wave
`Complete` + direct domain children `phase>=Planned`, NOT `allChildrenCaptured`), `namespaceManifestSpec`
targets. Integration: the three gate suites above.

---

## 10. Touch-list (files)

- `internal/controllers/snapshot/controller.go` — replace capture path with the SDK recipe; drop
  content `Create` + `boundSnapshotContentName` write.
- `internal/controllers/snapshot/snapshot_adapter.go` — **new** `NamespaceSnapshotAdapter`.
- `internal/controllers/snapshot/parent_graph.go` + `volume_capture.go` → **new** `planNamespaceChildren`
  (reuse the mapping/selector/weight logic; emit `[]ChildSpec`); retire only the bespoke
  *content-coupled* bits (the root no longer creates content), NOT the capture logic.
- `internal/controllers/snapshot/orphan_pvc_volume_snapshot.go` — orphan CSI wave
  (`reconcileOrphanPVCVolumeSnapshotChildLeaves`: per-PVC MCR + VS + leaf content + `childrenSnapshotRefs`
  ref + `residualVolumeCapture` latch) is **preserved**; only its child emission moves under
  `EnsureChildren` (§6.2). Do not rewrite the CSI capture path.
- `internal/usecase/root_capture_run_exclude.go` — `BuildRootNamespaceManifestCaptureTargets` feeds
  `namespaceManifestSpec` (§6.3); keep the wave-barrier exclude-set.
- `internal/controllers/snapshot/capture.go` → `namespaceManifestSpec` (targets) fed to
  `EnsureManifestCapture`; retire bespoke MCR create/drive (binder owns MCP chase).
- `internal/controllers/genericbinder/controller.go` — confirm `IsRootSnapshot` + root content
  create/bind path covers `Snapshot`; ensure it is the only `boundSnapshotContentName` writer.
- `internal/controllers/genericbinder/import.go` — extend to root import (PR-D).
- `pkg/unifiedbootstrap/pairs.go` — **add** `"Snapshot"` to `DomainCaptureSnapshotKinds` (keep it in
  `DedicatedSnapshotControllerKinds`; the two sets overlap — see §5.1).
- `pkg/unifiedruntime/syncer.go` — root no longer takes the fully-dedicated short-circuit
  (`:154-162`); it now flows to `MarkDomainCaptureKind` + `AddWatchForPair` (`:181-183`), respecting the
  activator ordering gate (`:176-180`).
- `cmd/main.go` — the root planning controller still registers, but now content-free; binder watch set
  includes the root.
- Tests as in §9.

---

## 11. Open questions to confirm before PR-B

1. `PublishSnapshotSource` + `snapshotSource` enum accept `Namespace` end-to-end — **likely already
   done** (`w5-api` is marked complete and the ADR root example uses `snapshotSource.kind: Namespace`).
   Confirm the enum + consumers (d8) rather than treating this as a PR-B blocker.
2. Does `EnsureManifestCapture`'s `ManifestCaptureSpec` allow a namespace target set (option 7/6.3-a)
   without SDK changes, or is a small SDK addition needed?
3. Root import: move to binder (preferred) vs keep in `snapshot/import.go` calling the binder
   materializer — decide before PR-D (affects d8's namespaced import consumption).
