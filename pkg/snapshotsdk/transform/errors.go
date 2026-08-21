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

package transform

import "errors"

// The restore failure vocabulary is part of the manifests-with-data-restoration wire contract: every
// server of that subresource — the core compiler and every out-of-process domain facade — wraps its
// failures in one of these four sentinels, and the shared status mapping turns them into the HTTP status
// a client sees. Defining them here rather than per server is what makes "the same failure answers the
// same way on every group" checkable instead of aspirational: a facade that invents its own error
// silently degrades to 500 and clients cannot tell a not-yet-ready snapshot from a broken one.
//
// Their messages are lowercase prefixes of the wrapped message (`fmt.Errorf("%w: ...", Err...)`) and are
// visible to clients, so they are stable text, not debugging aids.
var (
	// ErrNotFound is the addressed snapshot, node or filtered object does not exist (404).
	ErrNotFound = errors.New("not found")
	// ErrNotReady is a resolvable subject that is not usable yet — an unfinished capture leg, an
	// unbound content, a snapshot mid-recapture (409). Restoring from it would hand out stale data.
	ErrNotReady = errors.New("not ready")
	// ErrContractViolation is a state the model forbids: a broken back-reference, an empty checkpoint,
	// a data binding that points at the wrong artifact kind (409). It is always fail-closed — never a
	// partial answer.
	ErrContractViolation = errors.New("contract violation")
	// ErrBadRequest is a malformed or self-contradictory request (400), decided before any cluster read.
	ErrBadRequest = errors.New("bad request")
)
