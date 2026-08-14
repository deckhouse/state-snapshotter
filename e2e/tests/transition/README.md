# Transition e2e — new snapshot stack beside a running data module

Manual, developer-run scenario that brings `state-snapshotter` + `storage-foundation` up **on one dev
cluster that is already running the legacy snapshot stack** — `snapshot-controller` plus the
`storage-volume-data-manager` module (`svdm` below, an abbreviation of that module name) — without
deleting the legacy workload and **without turning the data module off**.

The two legacy modules are in different situations, and the scenario treats them differently:

- `snapshot-controller` is **superseded**. Its own Helm chart stops rendering workload once
  `storage-foundation` is enabled, leaving only its deprecation alert, and `storage-foundation` takes
  ownership of the CSI VolumeSnapshot CRDs it installed.
- `storage-volume-data-manager` **stays**. It keeps serving its own API group
  (`storage.deckhouse.io`), its own DataExport/DataImport resources and its own volume protection
  while the new stack runs beside it. Phase C asserts the flip left every bit of that untouched: same
  CRDs, same resources, same finalizer, same bytes served.

It is a **separate Ginkgo suite** (own `cluster_config.yml`, own bootstrap) because the main
state-snapshotter suite brings its cluster up with `storage-foundation`/`state-snapshotter`
already enabled — the opposite of what this scenario needs. All module lifecycle
(enable / MPO-retag / order) is driven at runtime from the test.

## Scope

- **In scope:** the deprecated snapshot-controller running **standalone with the extended (sf) CRDs**
  — the bundled vanilla external-snapshotter must still bind a Capture-mode VolumeSnapshot against
  them (the "old controller + new CRDs" check); its module Helm-guard behaviour (it stops rendering
  everything but its deprecation alert once storage-foundation is enabled) **and the firing
  deprecation alerts themselves** (built-in `ModuleIsDeprecated` + the custom
  `D8SnapshotControllerModuleDeprecated`), together with the negative half — no deprecation alert may
  fire for `storage-volume-data-manager`, which is not deprecated;
  **the legacy epoch surviving the flip untouched**: both legacy CRDs keep their identity, the
  DataExport and DataImport created in phase B are the same objects, the exported PVC keeps that
  module's finalizer, the export started in phase B still serves the same bytes afterwards, and the
  module keeps running its own workload; the export's **teardown under the new stack** (deleting it
  recovers the source PVC from `Lost` to `Bound`); CSI snapshot / DataExport-DataImport / restore data
  integrity across the flip, **a full storage-foundation DataExport+DataImport served on its own group
  after the flip**, and the existing state-snapshotter e2e on the same cluster.
- **Out of scope (tested by the runtime team, covered by canary channel rollout):** Deckhouse
  `requirements.deckhouse`/`requirements.modules` gating, `ModuleRelease` Pending→activation,
  bundle auto-enable. This suite runs on a **dev** Deckhouse build, which does not enforce
  requirements — so `>= 1.76.9` and `storage-foundation >= 1.0.0` gates are intentionally NOT
  exercised here.

## Running

```bash
cd e2e
E2E_RUN_TRANSITION=true \
TEST_CLUSTER_CREATE_MODE=<as for the main suite> \
  <plus the image-tag vars below> \
  go test ./tests/transition/... -v -timeout 180m
```

Without `E2E_RUN_TRANSITION=true` the suite is skipped entirely (nothing bootstraps). It is **not**
wired into CI — run it by hand from a workstation with dev-registry access.

### Reading the progress output

Run with `-v` (as above): every long wait is self-narrating. Each `By(...)` step prints the phase it
is in, and the waits emit `[transition HH:MM:SS] …` lines that poll every **3s** and log the current
state **immediately, then every 15s**, and once on success. So a hang is diagnosable from the trail
rather than an opaque timeout. In particular the Phase-B import step reports, at each tick:

- the `DataImport` conditions (e.g. `UploadFinished=True(...) Ready=True(...)`),
- the target PVC phase (`imported-data=Pending|Bound`),
- any populator staging PVC (`prime-<uid>=Pending`),
- the namespace's pods with a not-ready hint (`importer-…=Pending[…:ContainerCreating]`).

On timeout the failure message carries that same last-observed state. The whole import (upload →
importer `UploadFinished` → populator rebind → target PVC Bound) is bounded by
`E2E_TRANSITION_PROBE_TIMEOUT` (default `10m`).

## Resetting a reused cluster

The suite is re-runnable on the same dev cluster, but there is one sharp edge, and it applies to
clusters carrying an **older** `snapshot-controller` registration. Earlier builds of that module
declared a `requirements.modules.storage-foundation` dependency (the v0.2.0 build this suite installs
dropped that requirement — it installs standalone, see the phase-B row in the table below), and
**Deckhouse ignores a `ModulePullOverride` while the module is disabled**. So a cluster left with
snapshot-controller *registered* on such a gated build while disabled stays gated on
storage-foundation, and the next run's phase-B enable is webhook-denied:

```
admission webhook "module-configs...": the 'snapshot-controller' module depends on disabled module(s): storage-foundation
```

Phase A now fails fast with an explicit message when it detects this, instead of a cryptic phase-B error.

**`make transition-clean` handles both cases** (always run it between runs) via
`tests/transition/reset-cluster.sh`, which first checks whether the module is gated at all and
no-ops when it is not:

- if snapshot-controller is still *enabled*, it retags the MPO to `TRANSITION_SNAPC_LEGACY_TAG`
  (default `main`) and waits for it to re-register non-gated;
- if it is *disabled* and frozen on the gated build (an MPO alone is ignored, and Deckhouse checks a
  dependency's *effective* state — a module with no MPO/release has no version to deploy), it
  **redeploys the dependency chain**: it gives `state-snapshotter` then `storage-foundation` a
  ModulePullOverride, enables each and waits until it is effectively enabled, then enables
  snapshot-controller so its MPO re-pulls the legacy image, waits until it re-registers non-gated,
  and disables exactly what it transiently enabled. This path **requires the dependency tags** —
  export `STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE` and `STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE` (the
  same tags the run uses); without them the script errors out with guidance instead of hanging.

To run just the un-freeze without the full workload/namespace teardown:

```bash
# still-enabled case needs nothing extra; disabled+frozen case needs the dependency tags:
STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE=pr74 STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE=pr60 \
  make transition-reset-snapc          # or: sh tests/transition/reset-cluster.sh
```

`make transition-clean` also drops the legacy-group CRDs at the end. Deckhouse does **not** remove a
module's CRDs when the module is disabled, so they outlive the run; dropping them leaves the cluster
without CRDs nobody serves, and phase B reinstalls them together with the module.

## Environment variables

The scenario pins every module image via `ModulePullOverride.spec.imageTag`. Tags are chosen by
the runner (PR tags such as `pr123`/`mr456`, or `main`); nothing is hard-coded and nothing is
defaulted for the two modules that exist only in this scenario, so a run always records which build
it exercised. `sds-local-volume` is the only module that needs **two** image slots — a phase-B image
and a phase-C image the test retags to — because its current build requires `storage-foundation`,
which is disabled in phase B, whereas its legacy build requires only `snapshot-controller`.
`snapshot-controller` and `svdm` each need a single tag: snapshot-controller's v0.2.0 build has no
storage-foundation requirement, so it installs standalone in phase B and is only guard-flipped by the
phase-C enable, and svdm keeps serving its own API group throughout and is never retagged. Everything
else uses storage-e2e's standard `<MODULE>_MODULE_PULL_OVERRIDE`.

> **Tag format is validated up front.** `BeforeSuite` rejects any set image-tag / MPO env var that
> is not a plain-ASCII tag matching `mr<N>` / `pr<N>` / `main` (the dev-registry image tags). This
> fails the run immediately with a clear message instead of wedging a mid-run phase — it catches a
> prod `v*` tag (not in the dev registry) and, notably, a tag typed in a non-Latin keyboard layout
> (e.g. the Cyrillic `ает` for `main`). Set these variables with an EN/Latin layout.

| Variable | Type | Phase | Role |
|---|---|---|---|
| `E2E_RUN_TRANSITION` | scenario gate | all | must be `true`, else the whole suite is skipped |
| `SDS_NODE_CONFIGURATOR_MODULE_PULL_OVERRIDE` | standard | A (bootstrap) | sds-node-configurator image |
| `E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG` | scenario | B | snapshot-controller **v0.2.0** build (`Deprecated` + `D8SnapshotControllerModuleDeprecated` alert + extended storage-foundation CRDs). ONE tag: it has no storage-foundation requirement, so it installs standalone in phase B (no legacy/handoff split, no phase-C retag). Phase B asserts the vanilla controller works against the extended CRDs |
| `E2E_TRANSITION_SVDM_LEGACY_TAG` | scenario | B–D | svdm image. ONE tag, never retagged: the module serves DataExport/DataImport under `storage.deckhouse.io` — the group this suite calls the legacy one, hence the variable name — and keeps serving it while the new stack runs beside it. Its regular build is what phase B installs; no special build is needed |
| `E2E_TRANSITION_SDS_LOCAL_VOLUME_LEGACY_TAG` | scenario | B | sds-local-volume **legacy** image that depends on `snapshot-controller` (NOT storage-foundation). Required only when the data plane is enabled (sds-local-volume is the CSI backend for the data-plane steps); the current build requires storage-foundation and would be webhook-denied in phase B |
| `SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE` | standard | C | sds-local-volume storage-foundation-integrated image — MPO is retagged to this after the flip (default `main`). NOTE: the suite repoints this var at the legacy tag for phase B (the storage-e2e StorageClass testkit reads it when it lazily enables sds-local-volume), then restores it here — so during phase B its effective value is the legacy tag, by design |
| `STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE` | standard | C | state-snapshotter image (new stack) |
| `STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE` | standard | C | storage-foundation image (new stack) |
| `E2E_TRANSITION_STORAGE_CLASS` | scenario | B–D | snapshot-capable StorageClass for the data-plane PVCs; **unset ⇒ all data-plane steps are skipped** |
| `E2E_TRANSITION_VS_CLASS` | scenario | B–D | VolumeSnapshotClass for the CSI snapshots; **unset ⇒ all data-plane steps are skipped** |
| `E2E_TRANSITION_PROBE_IMAGE` | scenario | B–D | probe-pod image (needs `sh` + `sha256sum`); default `busybox:1.36` |
| `E2E_TRANSITION_PROBE_TIMEOUT` | scenario | B–D | Go duration bounding how long a probe pod may take to reach Running. For the import probe this budgets the WHOLE import completion (upload → importer `UploadFinished` → populator rebind → target PVC Bound → schedule). Default `10m` |

### Example

```bash
export E2E_RUN_TRANSITION=true

# Phase A (bootstrap): only sds-node-configurator is enabled from cluster_config.yml.
export SDS_NODE_CONFIGURATOR_MODULE_PULL_OVERRIDE="main"

# Phase B (legacy stack, driven at runtime):
# snapshot-controller v0.2.0 build (Deprecated, extended CRDs, no sf requirement) — installs
# standalone here; ONE tag, no phase-C retag.
export E2E_TRANSITION_SNAPSHOT_CONTROLLER_TAG="pr<N of the snapshot-controller v0.2.0 PR>"
# svdm image; it stays on this build for the whole run.
export E2E_TRANSITION_SVDM_LEGACY_TAG="main"
# sds-local-volume legacy image (depends on snapshot-controller, NOT storage-foundation). Required
# only when the data plane is enabled (see below). Its current build requires storage-foundation and
# would be webhook-denied in phase B.
export E2E_TRANSITION_SDS_LOCAL_VOLUME_LEGACY_TAG="<dev tag of an sf-independent sds-local-volume build>"

# Phase C (bring the new stack up beside the running data module):
export STATE_SNAPSHOTTER_MODULE_PULL_OVERRIDE="pr<N>"
export STORAGE_FOUNDATION_MODULE_PULL_OVERRIDE="pr<N>"
export SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE="pr<N>"          # sf-integrated build; retagged after the flip

# Data-plane (needed to exercise PVC/VS/export/import/restore; unset ⇒ those steps are skipped):
export E2E_TRANSITION_STORAGE_CLASS="e2e-thin"
export E2E_TRANSITION_VS_CLASS="e2e-local-thin"
# export E2E_TRANSITION_PROBE_TIMEOUT="15m"                   # default 10m; bump for slow clusters
```

## Phases

- **A — bootstrap:** dev cluster with only `sds-node-configurator`; the four snapshot-stack modules
  are preseeded `enabled: false` (auto-activation blocker); assert the cluster is clean (no
  snapshot-stack workloads/namespaces).
- **B — legacy stack:** enable `snapshot-controller` (its single **v0.2.0** build — Deprecated, no
  storage-foundation requirement, so it installs **standalone** and ships the extended sf CRDs) then
  `svdm` and — only when the data plane is enabled — `sds-local-volume` on its
  **legacy image** (`E2E_TRANSITION_SDS_LOCAL_VOLUME_LEGACY_TAG`, depends on `snapshot-controller`,
  NOT storage-foundation; the current build would be webhook-denied here); create a PVC + pod, write deterministic data
  (checksum), create a CSI `VolumeSnapshot` and wait ready+bound. This doubles as the **"vanilla
  controller + extended CRDs" check**: assert the served VolumeSnapshot CRD carries `spec.mode`, that
  the API server defaulted the VS to `mode=Capture`, and that the bundled vanilla external-snapshotter
  still bound it. Then DataExport the source PVC and download over the svdm HTTP API (not `d8`);
  DataImport/upload into a new PVC; CSI-restore a PVC from the snapshot; verify every checksum; keep
  everything. **The export is left live on purpose** — phase C reads it back across the flip.
- **C — flip beside the data module:** enable `state-snapshotter` → `storage-foundation` **without
  disabling** either legacy module and without touching svdm's image. The UIDs of the CSI CRDs and of
  both legacy CRDs are captured immediately before the enable, along with the identity of the live
  legacy epoch (the phase-B DataExport and DataImport, and the finalizer the export holds on the
  source PVC) — recording the finalizer up front is what keeps "still there afterwards" from passing
  on an epoch that never had one. Then assert:
  - `snapshot-controller` renders no workload any more (Deployments/Services drain to zero) — its own
    chart guard. `svdm` is deliberately **not** in that check: it has no such guard and must not gain
    one;
  - **the legacy epoch is untouched**: both legacy CRDs still exist, stay Established and keep their
    UID (deleting either cascades away every DataExport/DataImport a user created through that
    module, and reinstalling the CRD brings none of them back); the phase-B DataExport and DataImport
    are the same objects; the source PVC still carries svdm's finalizer; the phase-B export still
    serves the marker with a matching checksum; svdm still runs its own workload and both modules are
    Ready side by side. That same spec then tears the export down under the new stack — the CR goes
    away and the source PVC returns from `Lost` to `Bound`, so nothing in-flight crosses into phase D;
  - the storage-foundation DataExport/DataImport CRDs **arrived with the flip** and are Established
    (they are created by it, so there is no earlier UID to compare against; their absence beforehand
    is not asserted, because a disabled module's CRDs stay in the cluster and a reused cluster may
    still carry a previous run's copies);
  - the deprecation alerts fire for `snapshot-controller` (built-in `ModuleIsDeprecated` + custom
    `D8SnapshotControllerModuleDeprecated`) and **none** fires for `svdm` — checked after the positive
    ones, so alert evaluation is known to have caught up.

  Last in the phase (data plane only), **retag `sds-local-volume`**
  legacy→`SDS_LOCAL_VOLUME_MODULE_PULL_OVERRIDE` (the storage-foundation-integrated build) now that
  storage-foundation is up — the only retag in the scenario — so the phase-D data steps run against
  the sf-integrated CSI path.
- **D — invariants:** every tracked CRD (CSI `volumesnapshots`/`…contents`/`…classes` + the legacy
  `dataexports`/`dataimports.storage.deckhouse.io`) stays **Established with the same UID captured
  just before the flip** — for the CSI ones that proves the handoff re-applied them in place instead
  of delete+recreate (which would cascade-delete instances), for the legacy ones that the new stack
  left the neighbouring module's resources alone; the storage-foundation CRDs the flip created are
  Established too. The served schemas are checked for their
  storage-foundation marker fields (`spec.mode` on VolumeSnapshot, `targetRef.group` on DataExport,
  `spec.mode` on DataImport) so the served CRD is the extended/unified shape, not a vanilla
  reinstall. Full byte-for-byte CRD-manifest parity vs the repo YAML is **not** an e2e concern (the
  API server augments the live CRD with defaults/pruning/managedFields, so a manifest hash would
  never match) — it is verified by **storage-foundation CI** (`hack/check-consumer-crds.sh`, which
  diffs the CRDs it shares with its consumers). Additionally:
  the phase-B ready+bound VolumeSnapshot is untouched (no new-domain labels/status); all checksums
  still match, incl. a fresh CSI restore from that snapshot after the flip; a brand-new
  PVC/VS reaches ready+bound under the new controller; **a full DataExport+DataImport is served
  end-to-end by storage-foundation on its own group** (export → download → import → checksum) beside
  the live neighbour; then the existing state-snapshotter e2e passes on the same cluster. The deeper
  state-snapshotter *domain* path (Snapshot + `processed`/`managed` + SnapshotContent via the
  d8/domain SDK) is left to that suite.
