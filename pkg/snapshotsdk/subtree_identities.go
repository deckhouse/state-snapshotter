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

package snapshotsdk

// SubtreeManifestIdentity is one object identity captured somewhere in a snapshot subtree, as returned by
// the snapshotcontents/<name>/subtree-manifest-identities service subresource. It carries only identity
// (no manifest body): apiVersion/kind/namespace/name plus uid (to distinguish a recreated object of the
// same name). It is the wire contract SHARED by the service handler (server side) and
// SubtreeManifestIdentities (client side) so both marshal/unmarshal one definition. The matching key for
// exclude computation is apiVersion|kind|namespace|name (uid disambiguates a recreated object).
type SubtreeManifestIdentity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
}

// SubtreeManifestIdentitiesResponse is the JSON body of the subtree-manifest-identities subresource: the
// flat, de-duplicated set of object identities captured across the addressed SnapshotContent's ENTIRE
// subtree — its own node ManifestCheckpoint plus every descendant reached via childrenSnapshotContentRefs.
// It is FAIL-CLOSED: the server returns 409 (never a partial list) while any MCP in the subtree is not
// Ready, or if any object is double-captured across nodes. Consumers use it to compute an aggregator's
// manifest exclude set (base − subtree identities) before creating the aggregator MCR.
type SubtreeManifestIdentitiesResponse struct {
	Identities []SubtreeManifestIdentity `json:"identities"`
}
