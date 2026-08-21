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

package restore

import (
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

// The restore failure vocabulary belongs to the manifests-with-data-restoration contract, which the core
// compiler and every out-of-process domain facade serve alike, so the sentinels are defined once in the
// SDK. These names are the same values, not copies: errors.Is matches an error raised on either side of
// the delegation boundary, and a client gets one status for one kind of failure. They are kept as
// package-level names here because the compiler wraps them on nearly every failure path.
var (
	ErrNotFound          = transform.ErrNotFound
	ErrNotReady          = transform.ErrNotReady
	ErrContractViolation = transform.ErrContractViolation
	ErrBadRequest        = transform.ErrBadRequest
)
