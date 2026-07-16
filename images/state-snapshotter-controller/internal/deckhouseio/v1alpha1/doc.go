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

// Package v1alpha1 is a minimal LOCAL MIRROR of the upstream deckhouse.io/v1alpha1
// ObjectKeeper API. state-snapshotter reads/writes real ObjectKeeper custom
// resources (recycle-bin TTL GC of the root snapshot; see EnsureRootObjectKeeperWithTTL)
// that are owned and reconciled by deckhouse-controller, so this mirror must stay
// WIRE-COMPATIBLE: struct fields and json tags are copied verbatim from upstream.
//
// Source of truth:
//
//	github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1
//	@ v1.67.7-0.20251212134859-497a0dab9fc0
//
// Why a local copy instead of importing upstream: importing that single package
// pulls in the entire github.com/deckhouse/deckhouse module because its
// addKnownTypes registers dozens of unrelated types (ModuleConfig, Module,
// DeckhouseRelease, …) and linking the package drags them all in. We need only
// ObjectKeeper. Per agreement with the deckhouse-controller team, the ObjectKeeper
// schema is NOT expected to change and is NOT being extracted into a standalone API,
// so we mirror it here.
//
// DRIFT WARNING: this mirror is kept in sync MANUALLY. If deckhouse-controller ever
// changes the ObjectKeeper schema, update the types below (and the version above) to
// match. The wire-fidelity guard in objectkeeper_wire_test.go pins the JSON shape.
//
// The CRD (objectkeepers.deckhouse.io) is NOT shipped by us — deckhouse-controller
// owns it; envtest installs it manually in test/e2e/setup.go. That is why the
// upstream CEL/kubebuilder validation markers are intentionally omitted here.
//
// +kubebuilder:object:generate=true
// +groupName=deckhouse.io
package v1alpha1

//go:generate controller-gen object:headerFile=../../../../../hack/boilerplate.txt paths=.
