/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package domainsdk is the minimal, importable contract for building an out-of-process domain snapshot
// controller (its own binary + pod) that plugs into the state-snapshotter core, with the demo domain
// controller as the reference implementation.
//
// A domain controller owns its snapshot CRs (create ManifestCaptureRequest/VolumeCaptureRequest + child
// snapshots, write snapshot.status), but never SnapshotContent — the common controller in core owns that
// for all kinds. The domain pod also hosts its own aggregated API server for the restore subresources of
// its kinds; restore fetches the un-transformed BASE manifests from core (over the kube-apiserver
// aggregation layer) and applies a domain-specific mutation in-process.
//
// The SDK exposes the stable surface an external domain needs and nothing internal to the core compiler:
//
//   - Transformer (transformer.go): the restore extension point. Implement it next to your domain types
//     so the generic core compiler stays domain-free. RestoreNode/NodeResult are the minimal,
//     domain-facing view (just the owning snapshot identity + already-compiled child objects) — the
//     core compiler's richer internal node type is deliberately NOT exposed.
//
// The domain aggregated API server's authentication (front-proxy requestheader + TokenReview) and
// authorization (SubjectAccessReview) are delegated to k8s.io/apiserver genericapiserver (see
// internal/domainapi), so no bespoke front-proxy CA loading or CN allowlist lives in this SDK anymore.
//
// The snapshot-CR status/condition/identity contract lives in the already-public sibling package
// pkg/snapshot. See internal/controllers/demo + internal/domainapi for a full reference wiring of a
// domain reconciler set and aggregated API server built on this contract.
package domainsdk
