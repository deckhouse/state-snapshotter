# System specification (normative summary)

> **Scope of this file.** `spec/` holds the normative contract only (state machines, keys, invariants).
> Each section is a short normative summary that **references** the detailed design/ADR source of truth
> and never copies it. The delete-protection section below is the normative summary of
> [`../design/delete-protection-contract.md`](../design/delete-protection-contract.md) (its SSOT for the
> full model, rationale and invariants P1–P10).

## Delete protection (admission)

Direct user deletion of the internal nodes of a unified snapshot tree is blocked at the API server by an
admission delete-guard. The guard decides purely on one authoritative marker — it never reconstructs
membership from ownerReferences, finalizers, `followObjectRef`, names or Kind, and has **no** derived
fallback.

**Marker (single source of truth).**

- Label `state-snapshotter.deckhouse.io/delete-protected: "true"` = the object is an internal, managed
  node of a unified snapshot and MUST NOT be deleted by a direct user `DELETE`. Presence of the exact
  key=value pair is the only signal the guard reads.
- The **root `Snapshot` is never marked** — deleting it is the normal teardown entry point and drives the
  cascade. Foreign / standalone / vetoed (`…/managed=="false"`) objects are never marked, so the guard
  never blocks them.
- The marker is written by the authoritative write-path that introduces the node into the tree, **in the
  CREATE payload** for objects we create (core CRs, `SnapshotContent`, `ManifestCheckpoint`/chunks,
  `ObjectKeeper`, and the managed CSI `VolumeSnapshotContent` created by storage-foundation), and **by a
  patch completed before graph-edge publication** for the one adopted object (managed CSI `VolumeSnapshot`,
  in the `managed=true` latch). It is never computed by admission and is not removed during the normal
  lifecycle (legal deletion happens through exempt actors or break-glass with the marker preserved).

**Two independent admission invariants.**

1. **DELETE-deny.** A `DELETE` of a marked object is denied unless the requester is an exempt actor **or**
   the object carries the break-glass annotation `deckhouse.io/allow-delete: "true"`. The guard reads
   `oldObject` on DELETE. Allow ⇔ `not-DELETE ∨ exempt ∨ break-glass ∨ not-marked`.
2. **UPDATE marker immutability.** If `oldObject` was marked, a non-exempt actor may not remove the marker,
   change its value, or drop it by replacing the labels map. Other labels/annotations (including adding or
   removing the break-glass annotation before DELETE) may change freely.

**Break-glass.** `deckhouse.io/allow-delete: "true"` is a persistent, reversible operator override: adding
it does **not** remove the marker; the subsequent `DELETE` is admitted *because of the annotation*. It is
removable until the DELETE starts. Removing the marker is never a supported bypass.

**Exempt actors.** Verified initiators of routine teardown/GC/reclaim (by username in the admission
request, not merely a ServiceAccount in a Deployment). `system:masters` is **not** exempt. The exact list
lives in the delete-guard template and the design contract referenced above.

**Enforcement / rollout.** Helm value `deleteGuard.enforcement` (enum `Audit|Deny`, default `Audit`) drives
`validationActions`; the module ships in `Audit` so installation breaks nothing. Switching to `Deny` is a
user rollout action taken only after the backfill gate is proven — the one-shot, idempotent, cluster-wide
list-and-patch backfill reports that **every classifier-protected object already carries the marker**. The
migration classifier is the only place legacy provenance signals (ownerRef/finalizer/followObjectRef/name)
are read; admission never reads them.

**Fail-fast (orthogonal).** A protected node with `deletionTimestamp != nil` is treated as already lost;
degradation semantics (`ChildSnapshotDeleted` / `ChildSnapshotLost`) are unchanged by delete protection.

Finalizers are **not** a deletion prohibition (an object with finalizers only hangs in `Terminating`); the
prohibition is admission-based. Full model, invariants P1–P10 and rationale live in the design contract
referenced above. VCR/CSI execution semantics are out of scope of delete protection and are unchanged.
