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

package domainapi

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// coreSubresourceGroupVersion is the core controller's aggregated subresources API group/version. The
// domain controller fetches a node's own BASE manifests from core over the kube-apiserver aggregation
// layer (SA token, k8s-managed front-proxy). The restore path therefore never reads SnapshotContent /
// ManifestCheckpoint for manifest fetch. Snapshot reconcilers are content-free (no SnapshotContent
// watch/informer). DemoVirtualDisk restore reads SnapshotContent.status.dataRef via uncached APIReader
// (get-only RBAC, no informer) to build a VolumeRestoreRequest.
var coreSubresourceGroupVersion = schema.GroupVersion{
	Group:   "subresources.state-snapshotter.deckhouse.io",
	Version: "v1alpha1",
}

// CoreBaseManifestsFetcher fetches a single snapshot node's own (single-node) BASE manifests from the
// core aggregated API server. The result is the un-transformed, namespace-relative set of objects in
// that node's ManifestCheckpoint, which the domain controller then sanitizes + mutates in-process (demo
// restore transform) and recurses per-CR over the node's children (C9). It is NOT a whole-subtree dump.
type CoreBaseManifestsFetcher interface {
	// NodeBaseManifests returns the own-node base manifests for the snapshot identified by
	// (resource, namespace, name) via core's per-CR manifests-download. resource is the lowercase plural
	// (e.g. demovirtualdisksnapshots).
	NodeBaseManifests(ctx context.Context, resource, namespace, name string) ([]unstructured.Unstructured, error)
}

// CoreManifestsClient is a CoreBaseManifestsFetcher backed by the in-cluster REST client. All calls go
// through the kube-apiserver aggregation layer to the core API service (no bespoke mTLS client).
type CoreManifestsClient struct {
	rc rest.Interface
}

// NewCoreManifestsClient builds a CoreManifestsClient from an in-cluster rest.Config.
func NewCoreManifestsClient(cfg *rest.Config) (*CoreManifestsClient, error) {
	cfgCopy := rest.CopyConfig(cfg)
	// APIPath/GroupVersion/NegotiatedSerializer only satisfy rest.RESTClientFor; the actual request
	// uses AbsPath with a fixed group/version path (see NodeBaseManifests), so these are not used to build
	// the URL.
	cfgCopy.APIPath = "/apis"
	gv := coreSubresourceGroupVersion
	cfgCopy.GroupVersion = &gv
	cfgCopy.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	rc, err := rest.RESTClientFor(cfgCopy)
	if err != nil {
		return nil, fmt.Errorf("build core REST client: %w", err)
	}
	return &CoreManifestsClient{rc: rc}, nil
}

// NodeBaseManifests fetches GET
// /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/<ns>/<resource>/<name>/manifests-download
// from the core API service (the per-CR, single-node manifests) and decodes the returned JSON array of
// objects.
func (c *CoreManifestsClient) NodeBaseManifests(ctx context.Context, resource, namespace, name string) ([]unstructured.Unstructured, error) {
	if c == nil || c.rc == nil {
		return nil, fmt.Errorf("core manifests client is not configured")
	}
	raw, err := c.rc.Get().
		AbsPath(
			"/apis", coreSubresourceGroupVersion.Group, coreSubresourceGroupVersion.Version,
			"namespaces", namespace, resource, name, "manifests-download",
		).
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch node base manifests from core (%s/%s/%s): %w", namespace, resource, name, err)
	}
	return decodeManifestArray(raw)
}

// decodeManifestArray decodes a JSON array of Kubernetes objects into unstructured objects.
func decodeManifestArray(raw []byte) ([]unstructured.Unstructured, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list []unstructured.Unstructured
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode base manifests array: %w", err)
	}
	return list, nil
}
