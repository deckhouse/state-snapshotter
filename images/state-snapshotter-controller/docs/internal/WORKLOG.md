# WORKLOG (state-snapshotter-controller)

Chronological internal worklog: notable behavioral fixes and their rationale/coverage. Code comments
reference entries here by their short tag (e.g. "see WORKLOG vsc-reclaim-finalizer-lifecycle").

## Bugfix: vsc-reclaim-finalizer-lifecycle — durable per-node teardown + self-sufficient VSC reclaim

Fixes a durable-tree teardown leak where a captured `SnapshotContent` subtree could lose its physical
data artifacts (a dynamically-provisioned `VolumeSnapshotContent` and its `LVMLogicalVolumeSnapshot`)
when the root `ObjectKeeper` was deleted (TTL fire or namespace mass-deletion). Two independent gaps,
both closed:

1. **parent-protect is now held per-node until each content's OWN teardown.** Previously
   `GenericSnapshotBinderController` treated the namespaced bound `Snapshot` as the `parent-protect`
   parent: on bound-Snapshot deletion it latched `status.boundSnapshotDeleted=true` AND removed the
   content's `state-snapshotter.deckhouse.io/parent-protect` finalizer. But in the durable tree the
   Kubernetes GC owner of a content is its root `ObjectKeeper` (root content) or its parent
   `SnapshotContent` (child content) — not the bound Snapshot (`spec.snapshotRef` is a logical
   back-reference). Releasing the finalizer while the real owner was still alive let a later owner
   deletion remove the content without a persisted `deletionTimestamp` window, so its deletion handler
   and physical reclaim were not guaranteed to run. The same ownership confusion in
   `cascadeRemoveFinalizersFromChildren` stripped a direct child's finalizer after reclaiming only that
   child's own data, so a deeper descendant (root → VM content → disk content) could be GC'd without its
   handler ever running.

   Now: the binder only latches `boundSnapshotDeleted` (renamed `markBoundSnapshotDeletedOnContent`) and
   never removes the finalizer; the live `SnapshotContentController` ensures `parent-protect` first —
   even when `boundSnapshotDeleted=true` — which also self-heals a content left finalizer-less by the old
   code, then honors the bound-deleted early return; and the deletion path uses
   `ensureFinalizersOnChildrenForCascade` (a hard gate that ADDS/confirms `parent-protect` on every live
   direct child and never reclaims or strips a child, blocking parent finalization on any non-NotFound
   failure). Each content reclaims only its own MCP/VSC and removes only its own finalizer; ownerRef GC
   then deletes each child and the child's own handler repeats the sequence recursively — deterministic
   depth-N teardown with no test-only root finalizer.

2. **VSC reclaim stamps the CSI deletion annotation itself.** `reclaim.go`
   `reclaimVolumeSnapshotContent` now, in the same `MergeFrom` patch that flips `spec.deletionPolicy`
   `Retain→Delete`, stamps `snapshot.storage.kubernetes.io/volumesnapshot-being-deleted=yes`. The legacy
   external-snapshotter rule deletes a bound, `Delete`-policy VSC only when that annotation is present; it
   is normally stamped by the common controller during the bound `VolumeSnapshot` deletion lifecycle, but
   once the bound VS is already gone that stamp can be lost (content lookup/cache miss, binding mismatch,
   force-stripped VS finalizers, or a non-deletion-candidate VS) — permanently wedging the VSC and its
   physical `llvs`. state-snapshotter is the authority for these VSCs (it pinned them to `Retain` and now
   intentionally reclaims them), so stamping the annotation ourselves removes the dependency on that
   lifecycle-window stamp entirely. The reclaim computes `needsPolicy` and `needsAnnotation` independently
   and returns early only when BOTH are satisfied (this also recovers VSCs previously flipped to `Delete`
   but left unannotated), preserves unrelated annotations, canonicalizes a wrong value to `yes`, and keeps
   the idempotent `Delete`. Inert outside teardown: the annotation has no effect until a
   `deletionTimestamp` exists and `deletionPolicy` still gates the physical delete; for VCR-leg VSC-only
   contents it is redundant-but-harmless.

**Coverage.**

- `snapshotcontent/reclaim_test.go` — `Retain→Delete`+annotation+`Delete`; already-`Delete` recovery;
  wrong-value canonicalization preserving unrelated annotations; zero-Patch-but-idempotent-Delete no-op;
  terminating-VSC recovery.
- `snapshotcontent/finalizer_selfheal_test.go` — `boundSnapshotDeleted=true` + missing finalizer
  self-heals then early-returns (no stale-owner aggregation, no requeue).
- `snapshotcontent/deletion_path_test.go` — a deleting node reclaims its own VSC and removes only its own
  finalizer; a failed reclaim retains the finalizer and returns a retryable error.
- `snapshotcontent/controller_test.go` — parent teardown ensures/retains child finalizers, never strips
  them.
- Integration (`test/integration`): `snapshot_deletion_test.go` (binder latches `boundSnapshotDeleted`
  and RETAINS the content finalizer, incl. deterministic-name fallback and pre-Planned deletion),
  `snapshotcontent_cascade_test.go` (parent ensures child finalizers, removes only its own), plus the
  `boundSnapshotDeleted` field added to the test-CRD status schema in `setup_test.go`.
- e2e (`e2e/tests/volumedata_gc_test.go`, env-gated `E2E_VOLUME_DATA`, hard sds-local-volume dependency):
  a real data-bearing deep tree survives source-namespace deletion (every content naturally retains
  parent-protect; VSCs/llvs alive) under a large TTL, then deleting the root ObjectKeeper — after
  simulating the lost being-deleted stamp on the wired-ref VSCs — reclaims the whole tree including the
  physical llvs, with no synthetic finalizer barrier.

**Operational remediation (not code).** Already-wedged VSCs on affected clusters are NOT retro-healed by
this fix — their owning `SnapshotContent` is long gone, so reclaim never runs for them again. One-time
manual sweep: stamp `snapshot.storage.kubernetes.io/volumesnapshot-being-deleted=yes` on each wedged VSC
(its `deletionPolicy` is already `Delete`); the sidecar then completes the physical reclaim and removes
its bound-protection finalizer.
