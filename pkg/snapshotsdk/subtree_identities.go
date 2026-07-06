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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// subtreeManifestIdentitiesBasePath is the cluster-scoped SnapshotContent subtree-manifest-identities
// subresource path template. SnapshotContent is the single, core kind (all snapshot kinds bind to it), so
// the exclude endpoint always lives under the core subresources group.
const subtreeManifestIdentitiesBasePath = "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/%s/subtree-manifest-identities"

// ErrSubtreeIdentitiesPending is the fail-closed sentinel SubtreeManifestIdentities returns when the
// subtree exclude set is not yet complete: a child has not bound its content, or the subresource returned
// 409 (some subtree ManifestCheckpoint is not Ready). Callers requeue; they must never build a manifest
// MCR from a partial exclude.
var ErrSubtreeIdentitiesPending = errors.New("snapshotsdk: subtree manifest identities pending (subtree not fully persisted)")

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

// SubtreeManifestIdentities implements ManifestExclude: see the interface doc for the contract. It
// resolves each DIRECT child's bound SnapshotContent from the child snapshot's status and calls the
// subtree-manifest-identities subresource on each, unioning (de-duplicating on
// apiVersion|kind|namespace|name) the results into the aggregator's exclude set. It is fail-closed: any
// unbound child or any 409 from the subresource yields ErrSubtreeIdentitiesPending (never a partial set).
func (s *sdk) SubtreeManifestIdentities(ctx context.Context, t SnapshotAdapter) ([]SubtreeManifestIdentity, error) {
	if s.subresourceREST == nil {
		return nil, fmt.Errorf("snapshotsdk: SubtreeManifestIdentities requires a subresource REST client (New(..., WithSubresourceREST(...)))")
	}
	namespace := t.Object().GetNamespace()
	seen := make(map[string]struct{})
	out := make([]SubtreeManifestIdentity, 0)
	for _, child := range t.GetDomainCaptureState().ChildrenSnapshotRefs {
		contentName, err := s.resolveChildContentName(ctx, namespace, child)
		if err != nil {
			return nil, err
		}
		if contentName == "" {
			// Child exists but has not bound its content yet: its subtree is not persisted → fail closed.
			return nil, ErrSubtreeIdentitiesPending
		}
		ids, err := s.fetchSubtreeManifestIdentities(ctx, contentName)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			key := id.APIVersion + "|" + id.Kind + "|" + id.Namespace + "|" + id.Name
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, id)
		}
	}
	return out, nil
}

// resolveChildContentName reads a direct child snapshot's status.boundSnapshotContentName. A missing child
// (NotFound) is treated as pending (ErrSubtreeIdentitiesPending): the child has not materialized yet, so
// its subtree cannot be excluded and the caller must requeue rather than proceed with a partial exclude.
func (s *sdk) resolveChildContentName(ctx context.Context, namespace string, child SnapshotChildRef) (string, error) {
	gv, err := schema.ParseGroupVersion(child.APIVersion)
	if err != nil {
		return "", fmt.Errorf("snapshotsdk: parse child apiVersion %q: %w", child.APIVersion, err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gv.WithKind(child.Kind))
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: child.Name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return "", ErrSubtreeIdentitiesPending
		}
		return "", fmt.Errorf("snapshotsdk: get child snapshot %s/%s (%s): %w", namespace, child.Name, child.Kind, err)
	}
	name, _, err := unstructured.NestedString(u.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return "", fmt.Errorf("snapshotsdk: read boundSnapshotContentName on %s/%s: %w", namespace, child.Name, err)
	}
	return name, nil
}

// fetchSubtreeManifestIdentities GETs the subtree-manifest-identities subresource for one SnapshotContent
// and decodes the identity set. A 409 (Conflict) is the server's fail-closed signal (some subtree MCP not
// Ready) and is surfaced as ErrSubtreeIdentitiesPending.
func (s *sdk) fetchSubtreeManifestIdentities(ctx context.Context, contentName string) ([]SubtreeManifestIdentity, error) {
	path := fmt.Sprintf(subtreeManifestIdentitiesBasePath, contentName)
	body, err := s.subresourceREST.Get().AbsPath(path).DoRaw(ctx)
	if err != nil {
		if apierrors.IsConflict(err) {
			return nil, ErrSubtreeIdentitiesPending
		}
		return nil, fmt.Errorf("snapshotsdk: GET %s: %w", path, err)
	}
	var resp SubtreeManifestIdentitiesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("snapshotsdk: decode subtree identities for content %q: %w", contentName, err)
	}
	return resp.Identities, nil
}
