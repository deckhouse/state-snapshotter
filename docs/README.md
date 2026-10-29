---
title: "The state-snapshotter module"
description: "About what the state-snapshotter module does, why it's needed, and how to use it."
---

The `state-snapshotter` module is a critical infrastructure component of Deckhouse Platform designed to create and
restore snapshots of Kubernetes resource state. It is used by other Deckhouse Platform components that need to work
with state snapshots.

## Main features

The module lets you:

- Capture the state of Kubernetes resources in a namespace, including custom resources, ConfigMap, Secret, and other
  objects.
- Store the captured state for subsequent reading and restoration.
- Split large volumes of stored data into chunks.
- Restore resources from a previously created snapshot.
- Manage the retention period and cleanup of snapshot data.
- Use snapshots for backup, disaster recovery, and migrating state between clusters.
- Provide other Deckhouse Platform components with a mechanism for capturing and restoring resource state.

## How the module works

To create a snapshot, the user creates a Snapshot object in the required namespace. The module determines the objects
to include in the snapshot, captures their state, and stores the result for subsequent reading or restoration.

More on working with the module:

- [Working with Snapshots](user_guide.html): creating, downloading, uploading, and restoring snapshots with Snapshot
  and `d8`;
- [Backup integration](backup_integration.html): the HTTP API contract for external systems;
- [Working with snapshot archives](snapshot_archives.html): the archive format shared by `d8` and the Deckhouse web UI.

## Limitations

The module captures resources of its namespace. Of the cluster-wide resources, only the Namespace is captured.

{{< alert level="warning" >}}
Do not disable the `state-snapshotter` module: it is used by other Deckhouse Platform components that work with state
snapshots.
{{< /alert >}}
