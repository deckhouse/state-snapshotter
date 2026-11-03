---
title: "Working with snapshot archives"
description: "Snapshot archive format, tree layout, volume payloads, checksums, and validation before upload."
weight: 40
---

The `d8 snapshot download` command downloads a snapshot from the cluster into a directory, and `d8 snapshot upload`
uploads it from that directory. The archive format is shared by `d8` and the Deckhouse web UI. The commands in the
examples refer to `d8`. An external system can create or store an archive in the same format.

The archive format is not required to [create a snapshot](user_guide.html#creating-a-snapshot),
[track its status](user_guide.html#tracking-status), or transfer data through the
[backup integration API](backup_integration.html). It is required when a snapshot is passed as a file for use by
Deckhouse Platform tools. The format allows you to:

- Transfer a snapshot through file storage or on physical media
- Avoid connecting the external system and the cluster over the network
- Avoid requiring them to run at the same time

When another format is used, snapshots can be exchanged only through your own HTTP requests to the node API described in
[Snapshot tree and node API](backup_integration.html#snapshot-tree-and-node-api).

This specification describes a directory tree and its packaging into a `.zip` file. The existence of a packaging format
does not mean that every client accepts ZIP files. The `d8 snapshot upload -i` command accepts only an unpacked
directory, not a ZIP file. Unpack the ZIP archive before uploading with `d8`.

## Assembling an archive

Assemble the archive from the child nodes to the root so that child checksums are available before parent metadata is
finalized. To do this, follow these steps:

1. Lay out the tree: one directory per node, direct children under `snapshots/`.
1. Put each node's manifests into it, and its single payload if the node has a volume.
1. Fill in everything in every `snapshot.yaml` except the checksums.
1. Compute each node's `checksum`.
1. **Starting from the child nodes**, compute `childrenChecksum`, then `metadataChecksum`, and write `snapshot.yaml`.
   The order matters because the checksums of a child node go into the checksum of its parent node.
1. Check the directory: `d8 snapshot local describe <DIRECTORY>`.
1. If a single file is wanted, pack the checked tree into a `.zip`.

## Archive tree

The top level contains exactly one directory. Its name can be anything. It is the directory of the root node. Every
node, the root and a descendant alike, is a directory of one and the same shape:

```text {.nowrap-default}
my-namespace-snapshot/                      # The root node.
├── snapshot.yaml                           # The node's description and its three checksums.
├── manifests/                              # The node's own manifests, one object per file.
│   └── configmap_app.yaml
└── snapshots/                              # Direct child nodes. A node without child nodes has no such directory.
    ├── volumesnapshot_data-0/              # A VolumeSnapshot node with a block volume.
    │   ├── snapshot.yaml
    │   ├── manifests/
    │   │   └── persistentvolumeclaim_data-0.yaml
    │   └── data.bin
    └── volumesnapshot_files-0/             # A VolumeSnapshot node with a filesystem volume.
        ├── snapshot.yaml
        ├── manifests/
        │   └── persistentvolumeclaim_files-0.yaml
        └── data.tar
```

**When assembling an archive, follow these requirements:**

- Put a `snapshot.yaml` file in every node. If `snapshot.yaml` is missing from the root node, the entire archive is
  rejected during upload.
- Store in the `manifests/` directory, unchanged, all objects from the array that
  [`manifests-download`](backup_integration.html#reading-node-manifests) returned for this node, and only them. Store
  each object in a separate file. The `manifests/` directory must be present in every node and cannot be empty: the namespace
  Snapshot node holds at least the Namespace manifest, and a VolumeSnapshot node holds exactly one manifest of the captured
  PersistentVolumeClaim (PVC). Upload does not check this requirement: a node without manifests is accepted without
  an error.
- Use manifest file names that end in exactly `.yaml`. Files with the `.yml` extension or any other suffix are not
  manifests: they are not part of `checksum`, are not uploaded and are ignored without an error. These files are also
  absent from the list of skipped files shown after the walk.
- Use unique file names within `manifests/`. A scheme based on `kind` and the object name is usually enough, but the
  same `kind` can occur in different API groups. `d8` uses the name `<KIND>_<NAME>.yaml` and, if that `kind` exists in
  several API groups, adds the group: `<KIND>.<GROUP>_<NAME>.yaml`.
- Each manifest file must contain exactly one non-empty YAML or JSON document. Do not use a second document after `---`,
  empty documents or repeated keys in one mapping: on upload a manifest is parsed as a structured document rather than
  carried across as a sequence of bytes.
- Do not rename a manifest file after computing `checksum`: the relative path of the file goes into the checksum.
- Put each direct child node into its own subdirectory of `snapshots/`. If a node has no child nodes, the `snapshots/`
  directory is not created. The kind of a child node is determined only by its `snapshot.yaml`: the subdirectory name
  carries no meaning, but it must be unique among sibling nodes. `d8` uses the scheme
  `<KIND>_<NAME>` and, when the name is taken, appends `__<CHECKSUM_PREFIX>`.
- Do not put more than one payload into a node: `data.bin`, `data.bin.zst` or `data.tar`. The requirements are described
  in [The volume payload](#the-volume-payload).
- Do not create a `data/` directory: it is interpreted as a node with several volumes, and such nodes are rejected.

**When reading an archive created by another implementation, do not identify objects and nodes by their names.** An
object is identified only by the content of its manifest, and a node in a subdirectory only by its `snapshot.yaml`.

A subdirectory of `snapshots/` without that file is not a child node: it is skipped without an error and goes into the
list of skipped entries shown after the walk.

The archive is still rejected if that child node is counted in the parent node's `childrenChecksum`: the checksum stops
matching. So a node cannot be removed from a finished tree by deleting only its `snapshot.yaml`.

The `data.bin.d/` and `data.tar.d/` directories and the `identity.json` file remain after an unfinished download with
Deckhouse Platform tools and do not affect the node's content.

Additional data is ignored without an error and does not appear in the report. This applies, for example, to unknown
fields in `snapshot.yaml` and stray files in a node's directory. So you cannot use them to carry your own data.

The exception is names reserved for the payload: `data.bin*`, `data.tar` and `data`. Files and directories with such
names are always treated as part of the payload and are checked strictly: an extra file with such a name or invalid
content can get the node rejected. A file whose name only starts with `data.tar`, such as `data.tar.zst`, is not a
payload: it is ignored without an error, so do not compress a file node's payload.

**The following violations get the whole archive rejected before the cluster is accessed:**

| Violation | Rule |
|---|---|
| The root is not a namespace snapshot | When the whole archive is uploaded, the root node must contain `apiVersion: state-snapshotter.deckhouse.io/v1alpha1` and `kind: Snapshot`. If the root is a VolumeSnapshot, the error `archive root VolumeSnapshot/<NAME> is not a core Snapshot` is returned. This is a restriction of the upload mode, not of the archive format |
| A VolumeSnapshot node without volume data | A node with `apiVersion: snapshot.storage.k8s.io/v1` and `kind: VolumeSnapshot` must contain one of the payloads: `data.bin`, `data.bin.zst` or `data.tar` |
| `volumes` does not match the payload | If a payload is present, the `volumes` field must contain exactly one entry describing it. If there is no payload, `volumes` must contain no entries. A node cannot have more than one entry |

**A single volume can be uploaded on its own by naming the node.** The requirement on the root node applies only to
uploading a whole archive.

Any VolumeSnapshot node of an archive can be uploaded on its own if you specify the target node. In `d8`, this is done
with the `--node` flag:

```shell
d8 snapshot upload -n <NAMESPACE> -i <DIRECTORY> --node VolumeSnapshot/<NODE_NAME>
```

where:

- `<NAMESPACE>` is the namespace the volume is uploaded into;
- `<DIRECTORY>` is the path to the archive directory;
- `<NODE_NAME>` is the name of the VolumeSnapshot node in the archive.

The checks on `snapshot.yaml`, on the payload and on the checksums are the same as for uploading a whole archive.

The node is created in the cluster as a **new object of its own**; it cannot be attached to an existing tree.

The command verifies the checksums of the selected node's ancestors against `snapshot.yaml`. It does not look these
objects up in the cluster and does not create them.

## The volume payload

A node contains at most one payload. The `volumes` field in `snapshot.yaml` must match it:

| Node | Payload | `volumes` |
|---|---|---|
| No volume | none | The field is absent or contains `[]`; the two options are equivalent |
| Block volume | `data.bin` or `data.bin.zst` | exactly one entry, `volumeMode: Block` |
| Filesystem volume | `data.tar` | exactly one entry, `volumeMode: Filesystem` |

A VolumeSnapshot node can only be one of the last two rows. An empty `volumes: []` list does not affect
`metadataChecksum`, because the canonical form drops it just as it drops an absent field.

**Block volume.** An uncompressed payload uses `data.bin`. The file contains the whole block device as the export
returned it. zstd compression uses `data.bin.zst`. Other compression algorithms are not supported.

For a block node, `snapshot.yaml` records the payload size in the `rawSizeBytes` and `storedSizeBytes` fields described
in [The `snapshot.yaml` file](#the-snapshotyaml-file). The `size` value is the nominal volume capacity and is not
compared with the payload: the payload may be larger than `size`, because a storage system can round the provisioned
volume up. How to determine the `size` value is described in [Volume size](backup_integration.html#volume-size).

On upload, the payload is checked against the recorded sizes. For `data.bin`, the file size must equal
`storedSizeBytes`. For `data.bin.zst`, the declared frame sizes must add up to `rawSizeBytes`, and the decoded content
must have exactly that length. A discrepancy of even one byte gets the archive rejected. If a size field is absent, `d8`
measures the payload itself. The created volume must be at least as large as the payload, otherwise the upload fails.

**Filesystem volume.** For a filesystem volume, the payload size does not correspond to the volume capacity. On upload,
the payload size is taken from the entry headers of `data.tar`, and `rawSizeBytes` and `storedSizeBytes` are not used.

### zstd stream rules

These rules are the same for `data.bin.zst` and for compressed entries inside `data.tar`:

- Every frame declares its `Frame_Content_Size`.
- Skippable frames and dictionaries are not used.
- The structure of every frame is intact.
- The declared sizes add up to `rawSizeBytes` from `snapshot.yaml`, and for an entry inside `data.tar` to that entry's own
  `D8.snapshot.fs.rawSize`.

**Multiple frames make resuming a transfer easier.** An interrupted download or upload continues from the required
offset in the uncompressed data. That offset can be found in a compressed file only through the frame headers. The
declared sizes give the frame boundaries, so it is enough to decompress the required frame rather than the whole stream
from the beginning up to the point of the break.

A single frame for the whole volume is allowed, but every resume then has to decompress the stream from the beginning.
The frame size is not specified by the contract.

{{< alert level="warning" >}}
When data is passed to the compressor through a pipe, for example `cat data.bin | zstd`, the compressor does not know
the input size in advance and does not write `Frame_Content_Size`.

Pass the file to the compressor directly, for example with `zstd data.bin`. The size is then known and is written into
the frame header.
{{< /alert >}}

The frames are checked on upload. Some of the requirements are checked before the data transfer starts, others during
the transfer. If any of them is violated, the upload fails.

A payload compressed through a pipe still passes the [three checksums](#computing-checksums) and
`d8 snapshot local describe`: the data on disk is intact, but its size cannot be determined from the frame metadata.

### A filesystem volume is data.tar

`data.tar` itself is not compressed, only individual entries inside it can be compressed. The content of `data.tar` is
also defined by the archive format.

When a filesystem volume is exported, the files are downloaded one by one, so `data.tar` is assembled by the system that
creates the archive. An ordinary tar archive of the downloaded files will not do, without the service records described
below it is rejected.

Every regular file entry carries three PAX records:

| PAX record | Value |
|---|---|
| `D8.snapshot.fs.codec` | The entry's codec: `none` or `zstd`. All entries in `data.tar` must use the same codec. A `data.tar` with mixed codecs is rejected |
| `D8.snapshot.fs.originalPath` | The relative normalized path of the file inside the volume |
| `D8.snapshot.fs.rawSize` | The size of the entry before compression, in bytes, as a decimal number without leading zeros. For `none`, it must exactly match the size of the entry in `data.tar`. For `zstd`, it must match the size of the data after decompression |

Requirements for the path in `D8.snapshot.fs.originalPath`:

- No leading `/`, no backslash `\` and no drive letter.
- No empty segments and no `.` or `..` segments.
- No characters with a code below `0x20` and no `0x7f` character.
- The first segment must not be `.d8-meta`.
- The path must not change after `.` and `..` segments are normalized.

**The name of a file entry is determined by `originalPath` and the codec.** For `none`, `originalPath` itself is used;
for `zstd`, `.zst` is appended to it. The `originalPath` value must be unique among regular file entries: two entries
with the same path are rejected.

Additional PAX fields beyond the three described above are allowed, but they are not interpreted and have no guaranteed
semantics.

**Directories are passed as ordinary directory entries without PAX fields.** Their names are subject to the same
requirements as `originalPath`, except for the trailing `/`. That is why the `tar -C /volume -cf data.tar .` command
will not do: it creates the entries `./`, `./sub/` and so on, which are rejected because of the `.` segment. Build names
from the root of the volume, without a leading `./`.

**The order of entries in `data.tar` does not matter.** Parent directories do not have to be added explicitly. For
example, the entry `a/b/file` is accepted even without the `a/` and `a/b/` entries: the missing directories are created
when the file is written.

**For regular files, `mode`, `mtime` and the numeric `uid` and `gid` are restored.** Owner and group names (`uname` and
`gname`), as well as directory attributes, are not transferred.

Even for an explicit directory entry, permissions, owner and time are not restored. The directory is created as the
parent of the file inside it, and the other attributes of the entry are not used.

**Entries that are neither regular files nor directories are rejected.** These include symbolic links, hard links,
devices and named pipes (FIFOs). Empty directories are rejected too, because the upload protocol transfers files and
cannot recreate a directory that contains no files.

To skip unsupported entries, their skipping has to be allowed explicitly. It is disabled by default. In `d8`, use
the `--skip-unsupported-fs-entries` flag. Skipped entries are listed after the upload, because their absence means
data loss in the restored volume.

{{< alert level="info" >}}
The exception is an empty `lost+found` directory. It is always skipped without any extra permission and is not counted
as data loss. Such a directory is created when an ext4 file system is formatted and stays empty in a healthy volume.
Without this exception, no ext4 volume could be uploaded without allowing skipping. The last segment of the path is
matched, so the exception works at any depth and not only at the root of the volume.
{{< /alert >}}

## The snapshot.yaml file

The `snapshot.yaml` file describes the node and contains the data for verifying its content. It is checked before the
first write to the cluster. If the check of `snapshot.yaml` fails, the whole archive is rejected.

Example `snapshot.yaml` file of a VolumeSnapshot node:

```yaml
formatVersion: 3
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
name: data-0
namespace: my-app
uid: 5f6bd1ca-2c1a-4a2f-9f3f-000000000001
checksum:
  algorithm: sha256
  hex: 311996ce65e34f57fb9748e53a35c1f31df523ed533436ebce32a13f02f0f984
  short: 311996ce
childrenChecksum:
  algorithm: sha256
  hex: 926e82507890b7d5c7ad50f8cb7fd2172f81bfd2cf1d2ca86e3b644f66b02701
  short: 926e8250
metadataChecksum:
  algorithm: sha256
  hex: bc79311efba6b6048e0a3f8434b2b6e1b29b904eed144934067d412d6b9b8424
  short: bc79311e
volumes:
  - target:
      apiVersion: v1
      kind: PersistentVolumeClaim
      name: data-0
      namespace: my-app
      uid: 5f6bd1ca-2c1a-4a2f-9f3f-000000000002
    artifact:
      apiVersion: snapshot.storage.k8s.io/v1
      kind: VolumeSnapshotContent
      name: snapcontent-1
    volumeMode: Block
    storageClassName: my-storage-class
    size: 10Gi
    rawSizeBytes: 10737418240
    storedSizeBytes: 10737418240
```

**Write plain YAML scalars and do not include fields without values:**

- Specify `formatVersion`, `rawSizeBytes` and `storedSizeBytes` **as numbers without quotes**: `formatVersion: 3`, not
  `"3"`.
- An optional field without a value is **not included in the file**. Do not write it as `null`, `~` or an empty string.
  `metadataChecksum` is computed over the canonical form of the file, in which fields without values are absent. So an
  explicitly written `null` does not reproduce the checksum, and the archive is rejected.
- The order of keys does not matter: the checksums cover the field values, not the YAML document itself.

| Field | Presence | Rule |
|---|---|---|
| `formatVersion` | Required for new archives | The value is `3`. Archives with the value `2` are accepted as well |
| `apiVersion`, `kind`, `name` | Required | The identity of the node object. For Snapshot and VolumeSnapshot, it also determines the required content of the node |
| `namespace`, `uid` | Optional | Additional identity fields. Their values take part in computing the parent node's `childrenChecksum` |
| `sourceName`, `sourceObjectRef` | Optional | Information about the source object. `sourceObjectRef` either contains `apiVersion`, `kind` and `name`, or is absent entirely |
| `checksum`, `childrenChecksum`, `metadataChecksum` | Required | The checksums of the node's content, of its direct child nodes and of `snapshot.yaml` itself |
| `volumes` | Depends on the payload | Describes the volume payload. The rules are given in [The volume payload](#the-volume-payload) |

**`formatVersion`.** The current version is `3`, and `d8` writes it starting with version 0.33.16. Version `3` differs
from version `2` only by the `rawSizeBytes` and `storedSizeBytes` fields of a `volumes` entry. An archive with version
`2` is accepted: the payload size is measured on upload. An archive with any other `formatVersion` is rejected. A file without `formatVersion`
belongs to the format used before versioning was introduced. Its metadata is not integrity-protected. Do not create new
archives without `formatVersion`. By default, `d8 snapshot upload` and `d8 snapshot local describe` reject such
archives. The `--allow-unauthenticated-legacy` flag permits inspection or import only for trusted legacy archives, but
cannot distinguish a genuinely old archive from tampered metadata.

**`apiVersion`, `kind`, `name`, `namespace`, `uid`.** For a child node, `apiVersion`, `kind` and `name` must match the
values in the parent node's reference. For the root node, use the node's own values: Snapshot for a namespace
snapshot, or VolumeSnapshot or a domain kind for an individual-resource snapshot or a selected subtree.
Only the namespace Snapshot node owns the Namespace manifest. The whole-archive restriction of `d8 snapshot upload`
is described in [Archive tree](#archive-tree). If `namespace` and `uid` are specified, their values must match the ones
the parent node's
`childrenChecksum` was computed with. The requirements for the content of Snapshot and VolumeSnapshot nodes are given in
[Archive tree](#archive-tree), and the `childrenChecksum` algorithm in [Computing checksums](#computing-checksums).

**`sourceName`, `sourceObjectRef`.** `sourceName` contains the name of the captured source object, and
`sourceObjectRef` contains its `apiVersion`, `kind` and `name`. When assembling an archive yourself, you may omit these
fields. When re-uploading an archive created by Deckhouse Platform tools, keep them unchanged: their values go into
`metadataChecksum`.

**`checksum`, `childrenChecksum`, `metadataChecksum`.** `childrenChecksum` is specified even for a node without child
nodes. All three checksums have the same format:

- `algorithm` is `sha256`;
- `hex` is 64 lower-case hex characters;
- `short` is the first 8 characters of `hex`.

The `volumes` entry describes the volume in the payload. A DataImport is created from this entry on upload:

| Field | Rule |
|---|---|
| `target` | Information identifying the captured PVC: `apiVersion`, `kind` and `name` are required, and `namespace` and `uid` match the PVC. The manifest of this PVC is in this node's `manifests/` |
| `artifact` | Information about the stored content the node was bound to at capture time |
| `volumeMode` | The field is required. For `data.bin` and `data.bin.zst`, specify exactly `Block`; for `data.tar`, `Filesystem`. An empty or any other value is treated as a mismatch with the payload |
| `storageClassName` | The field is required and must not be empty |
| `size` | The capacity of the captured volume |
| `rawSizeBytes` | The size of the uncompressed payload in bytes. For `data.bin` and `data.bin.zst`, it is the length of the block device data; for `data.tar`, the sum of `D8.snapshot.fs.rawSize` over its regular-file entries |
| `storedSizeBytes` | The size of the payload file on disk in bytes: `data.bin`, `data.bin.zst` or `data.tar` |

For `artifact`, the `apiVersion`, `kind` and `name` fields are required and must not be empty. For example, for a
VolumeSnapshot node, specify the VolumeSnapshotContent from `status.boundVolumeSnapshotContentName`. These values are
not compared with anything and are not used on upload. `artifact` has the same structure as `target`, so `namespace` and
`uid` are also parsed and included in `metadataChecksum`. Do not specify `namespace` or `uid` for cluster-scoped
content.

The `size` value is required and must be positive. When assembling an archive from a cluster Snapshot, take it from
`status.data.size` of the volume node, not from `PersistentVolumeClaim.spec.resources.requests.storage` in the manifest.
The complete rules for passing the size are given in [Volume size](backup_integration.html#volume-size).
Additional requirements for block nodes are described in [The volume payload](#the-volume-payload).
On upload, `size` sets the capacity of the volume to create, so it must not be smaller than the captured volume's
capacity.

Write `rawSizeBytes` and `storedSizeBytes` for every node with a payload. A zero value is not written. Upload does not
require these fields: if a field is absent, `d8` measures the payload. For a filesystem node, `d8` does not use them on
upload, because the payload size is taken from the `data.tar` entry headers.

## Computing checksums

All three checksums are computed with SHA-256. Below is the exact algorithm the tools use to compute and verify them.

**`checksum` is the checksum of the node's content.** The computation includes all `manifests/*.yaml` files and the
single payload (`data.bin`, `data.bin.zst` or `data.tar`). It does not include `snapshot.yaml`, the `snapshots/`
subdirectory with all child nodes, or files in `manifests/` whose names do not end in `.yaml`.

1. For each included file, compute `sha256(<PATH> + 0x00 + <FILE_BYTES>)`. The path is given relative to the node's
   directory in UTF-8, for example `manifests/configmap_app.yaml` or `data.bin`. After the path, add one zero byte, then
   the file's bytes unchanged.
1. Sort the paths of the included files bytewise.
1. In that order, feed the raw 32 bytes of each file's checksum into the final SHA-256 computation.
1. Write the resulting value into `checksum.hex` in lower-case hexadecimal.

**`childrenChecksum` is the exact set of direct child nodes.** Fed into one SHA-256, in order:

1. Feed the ASCII string `d8-snapshot-children-checksum-v1` and one zero byte.
1. Feed the number of direct child nodes as a decimal string, length-prefixed.
1. For each child node, in the canonical order described below, feed seven values from its `snapshot.yaml`, each as a
   length-prefixed string:

   - `apiVersion`;
   - `kind`;
   - `namespace`;
   - `name`;
   - `uid`;
   - `checksum.hex`;
   - `childrenChecksum.hex`.

**A length-prefixed string** means that the length of the string in UTF-8 bytes is fed first, as an eight-byte
big-endian integer, followed by the bytes themselves. If `namespace` or `uid` is absent, an empty string is used: zero
length and no bytes after it.

For the canonical order, child nodes are sorted by the values of the `apiVersion`, `kind`, `namespace`, `name` and `uid`
fields joined with the byte `0x1F`, in that order. The comparison is bytewise. Two child nodes with the same values of
these fields are rejected.

For a node without child nodes, the checksum of the empty set is computed. Its value is always
`926e82507890b7d5c7ad50f8cb7fd2172f81bfd2cf1d2ca86e3b644f66b02701`. Reproducing this constant lets you verify that your
checksum implementation is correct.

The checksums of child nodes go into the checksum of the parent node, so `snapshot.yaml` files are written bottom up, as
described in [Assembling an archive](#assembling-an-archive).

**`metadataChecksum` is the checksum of the `snapshot.yaml` file.** The file is serialized into compact JSON with no
whitespace between tokens, and the `metadataChecksum` field is excluded entirely. SHA-256 is computed over the bytes of
this JSON. The key order is fixed and is not alphabetical:

| Level | Key order | Omitted when empty |
|---|---|---|
| Top level of the file | `formatVersion`, `apiVersion`, `kind`, `name`, `namespace`, `uid`, `sourceName`, `sourceObjectRef`, `checksum`, `childrenChecksum`, `volumes` | `formatVersion`, `namespace`, `uid`, `sourceName`, `sourceObjectRef`, `childrenChecksum`, `volumes` |
| Any checksum | `algorithm`, `hex`, `short` | — |
| `sourceObjectRef` | `apiVersion`, `kind`, `name` | — |
| A `volumes` entry | `target`, `artifact`, `volumeMode`, `storageClassName`, `size`, `rawSizeBytes`, `storedSizeBytes` | `volumeMode`, `storageClassName`, `size`, `rawSizeBytes`, `storedSizeBytes` |
| `target`, `artifact` | `apiVersion`, `kind`, `name`, `namespace`, `uid` | `namespace`, `uid` |

Fields from the "Omitted when empty" column are not written into the serialized JSON as `null` but are omitted from it
entirely. Numbers are written as decimal integers. In version `2`, a `volumes` entry has no `rawSizeBytes` and
`storedSizeBytes` keys.

This is a serialization rule, not a list of optional fields. `formatVersion` and `childrenChecksum` are required and
must be present in a valid file. In a valid file, only `volumes` on a node without a payload, optional identity
fields, and `rawSizeBytes` and `storedSizeBytes` with a zero value may be absent. All other keys are written even when their values are empty.

{{< alert level="warning" >}}
In strings, `"` and `\` are escaped. Particular attention should be paid to `<`, `>` and `&`: they are escaped as
`\u003c`, `\u003e` and `\u0026`. Everything else, including non-ASCII, is written as UTF-8.
{{< /alert >}}

The example in [The `snapshot.yaml` file](#the-snapshotyaml-file) produces exactly the `metadataChecksum` specified in
it, so you can use it to check your implementation of this serialization.

## Checking an archive before uploading

To check an archive locally, run the command:

```shell
d8 snapshot local describe <DIRECTORY>
```

where `<DIRECTORY>` is the path to the archive directory.

The command does not need a cluster and does not change the archive content.

The command checks:

- All three checksums of every node
- The fields of `snapshot.yaml` against the rules described above
- The agreement of `volumes` with the payload on disk

The command reads every byte of the payload, because the payload is covered by the node's content checksum. So the
command reads the whole archive and computes SHA-256 checksums, multi-terabyte volumes included. The check can take a
long time.

The command does not check:

- Whether `rawSizeBytes` and `storedSizeBytes` match the payload
- The contents of `data.tar`
- The zstd frames
- The presence of the required payload on a VolumeSnapshot node
- The kind of the archive's root node
- The packing into `.zip`, because the command checks a directory on disk

A successful check does not guarantee that the archive can be uploaded. For example, the command successfully checks an
archive with an ordinary tar archive or a `data.bin.zst` file compressed through a pipe, but
`d8` or the web UI rejects such an archive on upload.

## Packing the tree into a .zip archive

A `.zip` file contains the same tree in packed form. The layout inside it does not change: an entry's path matches the
file's path in the tree, and only `/` is used as the separator.

The web UI checks most of the requirements for a `.zip` when the archive is opened. Violating any of them gets the
whole archive rejected.

When creating a `.zip`, follow these requirements:

- The file name must end in `.zip`, in any case.
- The archive must contain exactly one top-level directory, the directory of the root node. Create a separate directory
  entry for it, even though the root can be determined from the path `<ROOT>/snapshot.yaml`.
- Entry paths must be unique. On a repeated path, the web UI reports no error and uses the last matching
  central-directory entry, so a change to a file may go unnoticed.
- Every entry must use the `stored` method: compression method `0` both in the local header and in the central
  directory, and the entry's size in the archive must match the size of the data. A compressed entry is rejected
  regardless of its size.
- Entries must not be encrypted, neither with ordinary nor with strong encryption.
- The archive must consist of a single part: disk numbers are `0`, the number of entries in the end-of-central-directory
  record matches the total number of entries, and the ZIP64 locator does not point to a second part.
- Entry names must be valid UTF-8 strings. If a name contains a byte `0x80` or above, the UTF-8 flag (bit 11) must be
  set. This flag can be set on all entries, including entries with ASCII names.
- Entry paths must not start with `/` and must not contain `..` or `.` segments, empty segments, a drive letter, a
  backslash, characters with a code below `0x20` or the `0x7f` character.
- An entry's local header must match its central-directory record: the name bytes, the compression method and the state
  of the `data descriptor` flag (bit 3) are the same. Sizes and `crc32` can be passed in a `data descriptor` after the
  entry data. In this case, the actual sizes must be given in the central directory: the web UI takes them only from
  there.
- A directory entry must end in `/` and have zero length both in the archive and in the data. A directory with content
  is recreated from the paths of the entries inside it, so a separate entry is needed only for an empty directory:
  without it, the web UI cannot tell an empty directory from a nonexistent one.
- If a field holds its limit value (`0xFFFFFFFF` for 32-bit fields or `0xFFFF` for the entry count field), the actual
  value must be given in the ZIP64 structure: in the `0x0001` extra field of the entry and in the ZIP64
  end-of-central-directory record with a locator.
- The number of entries in the central directory must match the declared one. Entry data must not run into the central
  directory. The end-of-central-directory record must end the file, and the declared comment length must reach exactly
  to the end of the file. The web UI accepts a correct non-empty comment, but when creating an archive, do not
  add a comment or any data before the first entry, including a self-extracting stub.

{{< alert level="info" >}}
The `crc32` value in the central directory is not checked against the data. The integrity of the archive is verified by
the three [SHA-256 checksums](#computing-checksums), not by the ZIP container.
{{< /alert >}}

## Where the format is defined

The directory-tree format described here is implemented by `d8` and the Deckhouse web UI. The module's HTTP API,
described in [Backup integration](backup_integration.html), does not define the on-disk format. An example of working
with an archive through `d8` is given in [Working with Snapshots](user_guide.html). When the format changes, the
`formatVersion` value in `snapshot.yaml` changes.

To check your implementation, run `d8 snapshot local describe` on the directory tree you created. Check the upload
with `d8 snapshot upload` into a suitable target namespace. Repeat these checks after updating the client.
