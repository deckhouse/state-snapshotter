# wave5 — Root `Snapshot` as an in-process SDK namespace-domain (design)

Status: **DESIGN / not implemented.** This is the design for the deferred wave5 todos
`w5-content-creation` + `w5-namespace-domain-sdk` + `w5-manifest-exclude-sdk` (the first two are one
change — see §2; the third is the reusable manifest-exclude capability they depend on — §6.3). Implement
attended, behind the envtest integration gate (`snapshot_root_lifecycle` / `snapshot_recreate` /
`snapshot_n1_boundary`). Companion execution log: `.cursor/plans/wave5_notes.md`.

> **Design pivot (2026-07-06).** An earlier draft put `sdk.EnsureManifestCapture` in the root's
> *planning* block (§4.2), symmetric with the demo aggregator. That is **wrong on timing**: the root's
> namespace MCR is **exclude-ordering-dependent** — its target set is «the whole namespace **minus**
> everything descendant snapshots already captured», and that exclude cannot be known at planning time.
> `BuildRootNamespaceManifestCaptureTargets` (`internal/usecase/root_capture_run_exclude.go`) has two
> hard preconditions the planning phase does not satisfy — (1) the root `SnapshotContent` must **already
> exist**, and (2) every **direct domain child's** bound content must be `subtreeManifestsPersisted=true`
> (the wave barrier `requireContentManifestsArchived`, else `ErrSubtreeManifestCapturePending`) — far
> later than `phase>=Planned`. Building the MCR earlier with a partial exclude double-captures
> child-owned objects (409 co-ownership).
>
> A **second draft** made the manifest leg a bespoke root carve-out (old «Option A») that read the
> cluster-scoped `SnapshotContent` directly from the reconciler. That is **also rejected**: the same
> aggregate-then-exclude need exists for any domain aggregator (a VM snapshot whose disk children capture
> part of its objects), and domain controllers have **no RBAC on `SnapshotContent`/`ManifestCheckpoint`**,
> so a content-reading carve-out is neither reusable nor available off-core.
>
> **Resolution — generalize the exclude into a reusable SDK capability (three parts, see §6.3):**
> (1) mirror the subtree barrier into namespaced status
> (`captureState.commonController.subtreeManifestsPersisted`, core-written) so any node has a cheap local
> «my subtree is archived» pre-gate; (2) add a service subresource
> `snapshotcontents/<name>/subtree-manifest-identities` (GET, recursive over the subtree, read-only,
> internal) that returns only object **identities** (`apiVersion/kind/namespace/name/uid`), fail-closed;
> (3) an SDK method that resolves a node's children content and calls (2) to return the subtree
> exclude-identity set. The root (and any aggregator) then computes `exclude` off that and calls the
> **normal** `sdk.EnsureManifestCapture(base − exclude)` — gated on (1), post-planning, but **no longer
> bespoke and no longer reading `SnapshotContent` from the reconciler**. The *timing* carve-out (the
> manifest leg runs after the subtree barrier, later than `phase=Finished`) remains; the **code**
> carve-out is gone. See §4.2, §6.3, §7. ADR overview already carries the endpoint + the RBAC rationale.

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

- **content-free** — it never creates/binds `SnapshotContent`, and (wave5 pivot) it never *reads* one
  from the reconciler either: the binder creates content, and the subtree-exclude the manifest leg needs
  comes from an SDK method backed by a service subresource (§6.3), not from a direct `SnapshotContent`
  read;
- **fully SDK-driven** — it plans children (`EnsureChildren`), publishes `status.snapshotSource`
  (kind=`Namespace`) via `PublishSnapshotSource`, drives the barrier (`MarkPlanned` → wait →
  `ConfirmConsistent` / `Reject`), **and** captures the namespace manifest via the ordinary
  `EnsureManifestCapture(base − exclude)`. The manifest leg keeps a *timing* carve-out only (it runs
  post-planning, gated on the subtree barrier — §6.3); it is no longer bespoke code;
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
  root is watched (§5) the same branch fires for it.

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
> not watch it today (§5). Hence the two todos cannot land independently; execute them as the single
> staged change in §7.

---

## 3. Current vs target (component responsibilities)

| Concern | Today (root, bespoke) | Target |
|---|---|---|
| `SnapshotContent` create/bind | `snapshot/controller.go` (`Create` + `Status().Update` of `boundSnapshotContentName`) | **binder only** (already the pattern for domains: `genericbinder/controller.go` create + `PatchUnstructuredBoundContentName`) |
| Children graph | bespoke `snapshot/parent_graph.go` | root domain builds `[]ChildSpec` → `sdk.EnsureChildren` |
| Namespace manifest | bespoke `snapshot/capture.go` `ensureManifestCaptureRequest` | `sdk.EnsureManifestCapture(base − exclude)`: `base` = namespace allowlist (root-owned enumeration), `exclude` = SDK subtree-identities method (§6.3). Post-planning + subtree-barrier-gated (`commonController.subtreeManifestsPersisted`), but **generic SDK code** — not a content-reading carve-out. Publishes `domainSpecificController.manifestCaptureRequestName` via the SDK |
| `status.snapshotSource` (Namespace) | not written | `sdk.PublishSnapshotSource` |
| Data leg projection onto content | bespoke `snapshot/capture.go` | root's own content = aggregator, **no data leg**; domain-child disk leaves use **binder** `ensureDomainContentLinks` (VCR→VSC→`status.data`). Orphan volume legs are the CSI path, separate row below |
| Orphan/residual PVC wave | `snapshot/volume_capture.go` + `orphan_pvc_volume_snapshot.go` (CSI VS + per-PVC MCR + leaf content + `childrenSnapshotRefs` ref) | **preserved**; emitted through `EnsureChildren` as `VolumeSnapshot` children (CSI path, no VCR — INV-ORPHAN1) (§6.2) |
| `boundSnapshotContentName` | dual writer (root + would-be binder) | **single writer: binder** |
| `commonController.manifestCaptured` (root RBAC latch) | `snapshot/ready_patch.go` `stampRootManifestCaptured` | stays core (binder `eagerInitCaptureLegs`/`markCaptureLegCaptured`); root RBAC carve-out retained (§6.4) |
| static-bind / import / delete-retain | `snapshot/{static_bind,import}.go` + `reconcileDelete` | binder's generic paths (`reconcileGenericStaticBind`/`reconcileGenericImport`) extended to the root (§6.5) |

**Shape match:** the root is an **aggregator** domain — namespace manifest + children, no single-PVC
data leg of its own — so it maps to the **DemoVirtualMachineSnapshot** pattern
(`images/domain-controller/internal/controllers/demo/virtualmachinesnapshot_controller.go`:
`EnsureChildren` + `MarkPlanned` + `ConfirmConsistent`), **not** the single-volume
DemoVirtualDiskSnapshot pattern. **Two deliberate divergences from the VM aggregator:**
1. The VM waits `allChildrenCaptured` + unfreeze/verify before `ConfirmConsistent` (crash-consistent
   group); the namespace domain has **no consistency action**, so its `phase=Finished` fires right after
   planning (orphan wave `Complete` + direct domain children `phase>=Planned`), without the
   `allChildrenCaptured` wait — see §4.2/§6.2.
2. The VM's manifest leg is a planning-time `EnsureManifestCapture` (per-object, exclude-free). The
   root's is the **same SDK call but late**: the namespace MCR is exclude-ordering-dependent (§6.3), so
   `EnsureManifestCapture(base − exclude)` runs post-planning, gated on the subtree barrier, once the SDK
   subtree-identities method can return a complete exclude set. Same SDK verbs as the VM; different
   *timing* (and a namespace-allowlist `base` minus a computed `exclude`, vs the VM's fixed per-object set).

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

  // NOTE: the namespace manifest leg is NOT here. It is exclude-ordering-dependent and cannot run at
  // planning time (§6.3) — it runs post-planning, below, as a gated SDK call (not bespoke code).

  sdk.MarkPlanned(ctx, adapter)                            // phase → Planned; unblocks binder

  // A namespace domain has NO consistency action (no freeze/unfreeze), so it Finishes as soon as
  // PLANNING is done — it does NOT wait for children dataCaptured / MCP execution / Ready (that is the
  // separate Ready gate, §5 / §8), and it does NOT wait for its OWN namespace manifest MCR (which is
  // built much later, see the manifest leg below). Finish gate (ADR "Корень как встроенный
  // namespace-домен", root example `phase: Finished` note):
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

  // ── Namespace-manifest leg (SDK exclude, gated — §6.3) ────────────────────────────────────────────
  // Runs on EVERY requeue (independent of and typically AFTER phase=Finished), because its exclude set
  // only becomes complete once every direct domain child's subtree is archived. It is now the ORDINARY
  // SDK call, gated on the mirrored barrier and fed by the SDK subtree-identities method — the
  // reconciler reads NO SnapshotContent.
  if !allDirectDomainChildrenSubtreeArchived(snap) {          // read child.commonController.subtreeManifestsPersisted (mirrored, §6.3-1)
      requeue                                                 // subtree barrier not passed yet
  }
  exclude, err := sdk.SubtreeManifestIdentities(ctx, adapter) // calls snapshotcontents/<child>/subtree-manifest-identities; fail-closed (§6.3-3)
  switch {
    errors.Is(err, ErrSubtreeIdentitiesPending):          requeue              // some subtree MCP not Ready → 409
    err != nil:                                            degrade Ready + requeue
  }
  base := namespaceManifestAllowlist(ctx, snap)              // dynamic/discovery enumeration of the namespace (root-owned; enumeration, not exclude)
  sdk.EnsureManifestCapture(ctx, adapter,
      ManifestCaptureSpec{Targets: subtract(base, exclude)}) // ordinary SDK MCR; writes manifestCaptureRequestName via the SDK
  // binder chases MCR→MCP (ensureSnapshotContentLinks); stampRootManifestCaptured latches
  // commonController.manifestCaptured (§6.4). These feed the Ready gate, never the phase gate.
```

`ConfirmConsistent` here is a phase-only transition (no verify step) — the namespace domain has nothing
to reconcile for consistency, unlike the VM aggregator whose `ConfirmConsistent` runs after
`allChildrenCaptured` + unfreeze/verify.

Everything the reconciler does today AFTER planning (create content, bind, publish content children,
run MCR→MCP *chase*, VCR→VSC handoff, mirror `Ready`) moves to the **binder** for the root, exactly as
it already runs for domains. The **namespace-manifest leg stays in the root reconciler only as a gated
SDK call sequence** (§6.3): the root still *decides* the namespace `base` (it owns the dynamic/discovery
clients) and *when* to fire (subtree-barrier gate), but the exclude computation, the MCR creation, and
the `domainSpecificController.manifestCaptureRequestName` write are all generic SDK; the binder chases
MCR→MCP as it already does for domain content. The reconciler reads no `SnapshotContent`.

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

### 6.3 Namespace manifest capture (one MCR for the whole namespace) — the exclude-ordering capability
This is the deepest carve-out and the reason the root manifest leg runs **late** (post-planning) and the
exclude had to be generalized into the SDK (see the top-of-file *Design pivot*).

**Why it cannot be a planning-time `EnsureManifestCapture`.** The demo `EnsureManifestCapture` creates a
per-object MCR whose target set is known immediately and needs no exclude. The root needs a single
**namespace** MCR (`namespacemanifest.SnapshotMCRName(uid)`) whose targets are «the whole namespace minus
everything descendant snapshots already captured». Historically that exclude was built by
`BuildRootNamespaceManifestCaptureTargets` (`internal/usecase/root_capture_run_exclude.go:67`) with two
hard preconditions the planning phase does not meet:

1. **The subtree must already be captured.** For every direct domain child it calls
   `requireContentManifestsArchived` (`...:214`), which fails closed with `ErrSubtreeManifestCapturePending`
   until the child's bound content reaches `subtreeManifestsPersisted=true` (the child's *entire* subtree
   persisted its manifests). That is strictly later than `phase>=Planned` (which only means the child
   finished its own planning), and therefore later than the root's own `phase=Finished` gate.
2. **Content had to exist to read the exclude.** The old builder `GET`s the root `SnapshotContent`
   (`...:82`) and walks `status.childrenSnapshotContentRefs` down to each child's MCP — a cluster-scoped
   read the reconciler should not be doing (and that domain aggregators, which have no `SnapshotContent`
   RBAC, cannot do at all).

Building the MCR earlier with a partial exclude re-captures descendant-owned objects on the root MCP —
the 409 duplicate / co-ownership violation the barrier exists to prevent (the code comment at
`...:118-126` spells it out for residual PVCs). So the manifest leg is **exclude-ordering-dependent** by
nature; that does not change. What wave5 changes is *how* the exclude is obtained.

**Resolution — a reusable SDK exclude capability (three parts).** The same aggregate-then-exclude need
exists for any domain aggregator (a VM whose disk children capture part of its objects), so the exclude
is lifted out of the root into the SDK:

1. **Mirror the barrier into namespaced status.** Add
   `status.captureState.commonController.subtreeManifestsPersisted` (bool) to the snapshot status,
   **core-written** — the binder mirrors `content.status.subtreeManifestsPersisted`, exactly as it already
   mirrors `manifestCaptured`/`data` in `genericbinder/domain_content.go`. This gives any node (root or
   domain) a cheap **local** «my subtree is archived» pre-gate with no cluster-scoped read. It is a
   distinct latch from `commonController.manifestCaptured` (which the RBAC hook reads, §6.4) — do not
   conflate the two; the exclude gate uses only `subtreeManifestsPersisted`.
2. **Service subresource `snapshotcontents/<name>/subtree-manifest-identities`** (GET, read-only,
   internal, not user-facing). The apiserver (with its own SA) recurses the subtree via
   `childrenSnapshotContentRefs`, reads each node's MCP, and returns only object **identities**
   (`apiVersion/kind/namespace/name/uid`; no bodies; no `resourceVersion` for now — additive later).
   **Fail-closed:** if any MCP in the subtree is not `Ready`, the whole call is `409`. It reuses
   `appendObjectsFromManifestCheckpoint` + `aggregatedObjectIdentityKey`
   (`internal/usecase/aggregated_namespace_manifests.go`); registered in
   `internal/api/archive_handler.go` `HandleAPIResourceListDiscovery` + routed in `SetupRoutes`. It lives
   on `SnapshotContent`, **not** `ManifestCheckpoint`, because domain controllers have RBAC on neither MCP
   nor generic content reads, but *can* be granted this one narrow subresource verb
   (`snapshotcontents/subtree-manifest-identities`, verb `get`). Server-side recursion is one round-trip +
   local reads + the apiserver's archive cache, vs N client calls each re-authorized (ADR rationale box).
3. **SDK method `SubtreeManifestIdentities(ctx, adapter)`.** It resolves the node's direct children
   content (`child.status.boundSnapshotContentName`) and calls (2), returning the subtree exclude-identity
   set — without the caller ever touching `SnapshotContent`/`ManifestCheckpoint` objects. Surfaces the
   fail-closed state as `ErrSubtreeIdentitiesPending` (caller requeues).

The root (and any aggregator) then: gate on (1) → `exclude` = (3) → `base` = namespace allowlist
(dynamic/discovery enumeration, still root-owned — this is *enumeration*, not exclude) → **ordinary**
`sdk.EnsureManifestCapture(ManifestCaptureSpec{Targets: base − exclude})`, which writes
`domainSpecificController.manifestCaptureRequestName` via the SDK and lets the binder chase MCR→MCP.
Matching key = `apiVersion|kind|namespace|name` (+`uid` to distinguish a recreated object). Fail-closed
throughout (a partial exclude is never used).

**What this retires.** The old bespoke root manifest leg (`ensureManifestCaptureRequest` +
`BuildRootNamespaceManifestCaptureTargets`'s cluster-scoped content/MCP walk *in the reconciler*) is
removed; the exclude computation moves **server-side** behind the subresource. The PR-A helper
`namespaceManifestSpec` is now **on** the root path (it shapes the `base` allowlist into
`ManifestCaptureSpec`). Old Options **A** (bespoke, content-reading), **B** (move the machinery into the
binder), and **C** (lazy exclude at MCP-archive time) are **superseded** by this capability: it is more
reusable than A, smaller blast radius than B, and keeps the fail-closed 409 guarantee (unlike C).

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
2. **PR-A2 (additive: the manifest-exclude capability, §6.3).** Land all three parts **additively,
   unconsumed by the hot path**: (1) the core-written `commonController.subtreeManifestsPersisted` mirror
   (binder), (2) the `snapshotcontents/<name>/subtree-manifest-identities` subresource + its RBAC grant to
   domain controllers, (3) the `SubtreeManifestIdentities` SDK method. The root still uses its old bespoke
   manifest leg here, so this PR changes no root behavior; unit/integration-test the subresource
   (fail-closed 409, recursion) and the SDK method standalone. (Order: PR-A ‖ PR-A2 both before PR-B.)
3. **PR-B (flip creation to the binder + consume the exclude capability).** In one commit: (a) add
   `Snapshot` to `DomainCaptureSnapshotKinds` (keep it in `DedicatedSnapshotControllerKinds`) so the binder
   watches it (§5), (b) delete the root's content `Create` + `boundSnapshotContentName` `Status().Update`,
   (c) switch the reconciler's **planning** path to the SDK recipe (`EnsureChildren` + `MarkPlanned` +
   `ConfirmConsistent`, §4.2), (d) switch the **namespace-manifest leg** to the SDK exclude capability
   (§6.3): gate on `commonController.subtreeManifestsPersisted` for every direct domain child,
   `exclude = sdk.SubtreeManifestIdentities`, `sdk.EnsureManifestCapture(base − exclude)`, and **delete**
   the bespoke `ensureManifestCaptureRequest` + the in-reconciler `BuildRootNamespaceManifestCaptureTargets`
   content/MCP walk. Because content creation is now binder-only from the same commit, there is no
   dual-writer window. Keep static-bind/import bespoke in this PR (still root-owned) to shrink blast radius.
   **Ordering hazard to gate hard:** the manifest leg now fires only when the mirrored barrier is set for
   all direct domain children and the subresource returns a complete (non-409) exclude set; verify the
   requeue path (`ErrSubtreeIdentitiesPending`) still converges once the binder, not the root, creates
   content.
4. **PR-C: route the (existing) orphan `VolumeSnapshot` wave through `EnsureChildren`.** Emission seam
   only — the CSI capture path, per-PVC MCR, invariants (INV-ORPHAN1/2), the `childrenSnapshotRefs` ref
   (Variant A) and the `residualVolumeCapture` latch are all **preserved**. Separate PR so it can be
   reverted independently. Gate on `snapshot_n1_boundary` + the two-PVC subtree spec.

   > **Implementation outcome (2026-07-06): PR-C lands as a no-op code change — the goal is already met by
   > PR-B.** Routing the orphan emission through a generic SDK verb is infeasible without a regression:
   > `reconcileOrphanPVCVolumeSnapshotChildLeaves` performs a **replace of the VolumeSnapshot-leaf
   > partition** (a leaf ref no longer desired is dropped from `childrenSnapshotRefs`, while the durable VS
   > object is never pruned — pinned by `TestEnsureOrphanPVCVolumeSnapshots_DurableVSNotPruned`). The SDK is
   > deliberately **delete-free** (additive union) and **kind-agnostic**, so it cannot express "replace only
   > the VolumeSnapshot-leaf partition" without leaking root-specific leaf semantics into the shared module,
   > and a naive `EnsureChildren` reuse would additionally (a) reset the planner's `excludedRefs` and (b)
   > flip the leaf's **deliberate non-controller ownerRef** to controller-owned. The single-field-correctness
   > the single-writer goal targets is already achieved by PR-B: `EnsureChildren` unions additively (so it
   > preserves the orphan leaves) and the orphan writer preserves the non-leaf domain refs — the two
   > partitions co-write `childrenSnapshotRefs` without conflict. The orphan `reconcile…ChildLeaves` write is
   > therefore kept in the controller (with an explanatory comment) instead of being moved to the SDK.
5. **PR-D: move static-bind + import into the binder.** Finalize "single content creator".

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
  root RBAC `stampRootManifestCaptured`). SDK reads, never writes (`adapter.go:23-63`). wave5 adds
  `subtreeManifestsPersisted` here (§6.3-1), also **core-written** (binder mirror of
  `content.status.subtreeManifestsPersisted`) — the SDK *reads* it in `SubtreeManifestIdentities`'s
  caller-side gate, never writes it.
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
- **Namespace MCR ordering** (§6.3) — the manifest leg is exclude-ordering-dependent; after PR-B it fires
  only when `commonController.subtreeManifestsPersisted` is set for every direct domain child **and**
  `sdk.SubtreeManifestIdentities` returns a complete (non-409) identity set. Regression risk: the leg
  fires before the subtree is archived and builds a partial-exclude MCR (409 co-ownership); or the
  subresource returns partial identities without failing closed. Gate: `snapshot_root_lifecycle` + the
  subtree exclude specs; unit-test the gate + `base − exclude` set difference.
- **`subtree-manifest-identities` subresource** (§6.3-2) — must be **fail-closed**: any MCP in the subtree
  not `Ready` → `409` for the whole call (never a partial identity list); recursion must cover the entire
  subtree, not just direct children; RBAC must expose it to domain controllers without granting MCP or
  generic content reads. Unit/integration-test recursion depth + fail-closed + RBAC scope.
- **RBAC latch** — keep `stampRootManifestCaptured`; regression = root never reaches
  `manifestCaptured=true` → binder never publishes MCP. Do not conflate it with the new
  `subtreeManifestsPersisted` latch (§6.3-1).

New unit coverage to add alongside implementation: `NamespaceSnapshotAdapter` write-discipline
(only domain half + source + children), `planNamespaceChildren` (weight layers + exclude veto +
orphan PVCs → `VolumeSnapshot` children, CSI path, no VCR), the `phase=Finished` gate (orphan wave
`Complete` + direct domain children `phase>=Planned`, NOT `allChildrenCaptured`), and the manifest-exclude
capability: the `subtreeManifestsPersisted` gate (requeue while unset), the subresource fail-closed 409
(any subtree MCP not Ready), `SubtreeManifestIdentities` resolving children content refs, and the
`base − exclude` difference. Integration: the three gate suites above + the subtree exclude specs.

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
- `internal/usecase/root_capture_run_exclude.go` — the exclude computation
  (`BuildRootNamespaceManifestCaptureTargets` content/MCP walk) moves **server-side** behind the new
  subresource (§6.3-2); its recursion + fail-closed 409 logic is reused there. The in-reconciler caller is
  removed. `base` allowlist enumeration (dynamic/discovery, exclude-free) stays root-owned.
- `internal/api/archive_handler.go` (+ route setup) — **new** `snapshotcontents/<name>/subtree-manifest-identities`
  subresource: server-side recursive identity walk (reusing `appendObjectsFromManifestCheckpoint` +
  `aggregatedObjectIdentityKey` from `internal/usecase/aggregated_namespace_manifests.go`), fail-closed
  409, registered in `HandleAPIResourceListDiscovery` (§6.3-2).
- `api/storage/v1alpha1/capture_state_types.go` — **new** `commonController.subtreeManifestsPersisted`
  (bool) mirror field (§6.3-1); controller-gen (CRD + deepcopy + docs).
- `internal/controllers/genericbinder/domain_content.go` — mirror `content.status.subtreeManifestsPersisted`
  into `commonController.subtreeManifestsPersisted` (alongside the existing `manifestCaptured`/`data`
  mirrors) (§6.3-1).
- `pkg/snapshotsdk/capture.go` — **new** `SubtreeManifestIdentities(ctx, adapter)` method (resolves child
  content refs, calls the subresource, returns exclude identities; surfaces `ErrSubtreeIdentitiesPending`)
  (§6.3-3).
- `hooks/` (domain RBAC) — grant domain controllers `snapshotcontents/subtree-manifest-identities` verb
  `get` (no MCP / generic content read); mirror the ADR RBAC rationale.
- `internal/controllers/snapshot/capture.go` → **delete** the bespoke manifest leg
  (`ensureManifestCaptureRequest` + the in-reconciler barrier-requeue loop, `:210-296`); replace with the
  SDK exclude call sequence (§4.2 "Namespace-manifest leg"): gate on `subtreeManifestsPersisted`,
  `sdk.SubtreeManifestIdentities`, `sdk.EnsureManifestCapture(base − exclude)`. `namespaceManifestSpec`
  (PR-A) now shapes `base` on the root path.
- `internal/controllers/genericbinder/controller.go` — confirm `IsRootSnapshot` + root content
  create/bind path covers `Snapshot`; ensure it is the only `boundSnapshotContentName` writer; still owns
  the MCR→MCP *chase* (unchanged).
- `internal/controllers/genericbinder/import.go` — extend to root import (PR-D).
- `pkg/unifiedbootstrap/pairs.go` — **add** `"Snapshot"` to `DomainCaptureSnapshotKinds` (keep it in
  `DedicatedSnapshotControllerKinds`; the two sets overlap — see §5).
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
2. ~~Does `EnsureManifestCapture`'s `ManifestCaptureSpec` allow a namespace target set without SDK
   changes?~~ **Resolved.** PR-A made `ManifestCaptureSpec` multi-target, so the SDK expresses the
   namespace MCR directly. The *ordering* problem (exclude needs a fully-archived subtree + a content read)
   is solved by the new SDK exclude capability (§6.3): the root calls
   `sdk.EnsureManifestCapture(base − exclude)` **late**, gated on `subtreeManifestsPersisted`, with
   `exclude` from `sdk.SubtreeManifestIdentities`. `sdk.EnsureManifestCapture` **is** on the root path now.
3. **New subresource RBAC (§6.3-2)** — confirm domain controllers can be granted
   `snapshotcontents/subtree-manifest-identities` verb `get` without any `ManifestCheckpoint` or generic
   `snapshotcontents` read, and that the apiserver SA can recurse the subtree. PR-A2 blocker.
4. Root import: move to binder (preferred) vs keep in `snapshot/import.go` calling the binder
   materializer — decide before PR-D (affects d8's namespaced import consumption).
