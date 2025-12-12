---
title: "The state-snapshotter module"
description: "About what the state-snapshotter module does, why it's needed, and how to use it."
d8Edition: ee
---

## What is the state-snapshotter module?

The `state-snapshotter` module is a **critical infrastructure component** in Deckhouse that provides state capture and restoration capabilities for Kubernetes resources. It allows you to create point-in-time snapshots of specific Kubernetes objects and restore them later.

### What does it do?

The module provides:

1. **State Capture**: Captures the current state of specific Kubernetes resources (CRDs, ConfigMap, Secret, etc.) into immutable checkpoints
2. **State Storage**: Stores captured state in a distributed format with chunking, optimized for large datasets
3. **State Restoration**: Retrieves and restores captured state through a native Kubernetes API
4. **Automatic Lifecycle Management**: Automatically manages checkpoint storage and cleanup using TTL-based retainers

### Why is it needed?

The `state-snapshotter` module is essential for:

- **Backup and Restore Operations**: Create backups of application state before important operations (upgrades, migrations, etc.)
- **Disaster Recovery**: Maintain point-in-time snapshots for disaster recovery scenarios
- **State Migration**: Capture state from one cluster and restore it in another
- **Audit and Compliance**: Maintain historical records of resource states for audit purposes
- **Integration with Other Modules**: Used by other Deckhouse modules (e.g., `virtualization`, `backup`) to capture and restore application state

### Why you should NOT disable it

⚠️ **Important**: The `state-snapshotter` module should **NOT** be disabled because:

1. **Critical Infrastructure**: It's a foundational component used by other Deckhouse modules for state management
2. **No Performance Impact**: The module only activates when explicitly requested via `ManifestCaptureRequest` resources - it doesn't run background processes
3. **Resource Efficient**: Uses minimal resources and only processes explicit capture requests
4. **Required for Operations**: Many Deckhouse operations (backups, snapshots) depend on this module
5. **Automatic Cleanup**: Automatically manages its own resources and cleans up old checkpoints based on TTL
