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

package api //nolint:revive // package name matches internal/api directory

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// domainSubresourceGroupPrefix is prepended to a domain snapshot's API group to address its aggregated
// subresources group. The domain controller registers its restore subresources under
// "subresources.<domain group>" (e.g. demo group "demo.state-snapshotter.deckhouse.io" ->
// "subresources.demo.state-snapshotter.deckhouse.io"), distinct from core's own
// "subresources.state-snapshotter.deckhouse.io" so the two APIServices never collide. Keep this in
// sync with the domain apiserver's served group (internal/domainapi handler).
const domainSubresourceGroupPrefix = "subresources."

// DomainRestoreClient delegates a domain snapshot subtree restore to the domain controller's
// aggregated apiserver. All calls go through the kube-apiserver aggregation layer (in-cluster SA
// token, k8s-managed front-proxy) — there is no bespoke mTLS client between pods. It implements
// restore.DomainSubtreeRestorer.
type DomainRestoreClient struct {
	rc     rest.Interface
	mapper meta.RESTMapper
	log    logger.LoggerInterface
}

var _ restore.DomainSubtreeRestorer = (*DomainRestoreClient)(nil)

// NewDomainRestoreClient builds a DomainRestoreClient from an in-cluster rest.Config. mapper resolves
// a domain snapshot GVK to its resource (plural) for the request path.
func NewDomainRestoreClient(cfg *rest.Config, mapper meta.RESTMapper, log logger.LoggerInterface) (*DomainRestoreClient, error) {
	cfgCopy := rest.CopyConfig(cfg)
	// APIPath/GroupVersion/NegotiatedSerializer only satisfy rest.RESTClientFor; the actual request
	// uses AbsPath with a per-call group/version (the domain group varies by kind), so the GroupVersion
	// set here is a placeholder and never shapes the URL.
	cfgCopy.APIPath = "/apis"
	gv := schema.GroupVersion{Group: domainSubresourceGroupPrefix + "domain", Version: "v1alpha1"}
	cfgCopy.GroupVersion = &gv
	cfgCopy.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	rc, err := rest.RESTClientFor(cfgCopy)
	if err != nil {
		return nil, fmt.Errorf("build domain REST client: %w", err)
	}
	return &DomainRestoreClient{rc: rc, mapper: mapper, log: log}, nil
}

// RestoreDomainSubtree fetches GET
// /apis/subresources.<group>/<version>/namespaces/<ns>/<resource>/<name>/manifests-with-data-restoration
// from the domain apiserver and decodes the returned JSON array of apply-ready objects.
func (c *DomainRestoreClient) RestoreDomainSubtree(ctx context.Context, gvk schema.GroupVersionKind, namespace, name, targetNamespace string) ([]unstructured.Unstructured, error) {
	if c == nil || c.rc == nil {
		return nil, fmt.Errorf("domain restore client is not configured")
	}
	resource, err := c.resourceFor(gvk)
	if err != nil {
		return nil, err
	}
	subGroup := domainSubresourceGroupPrefix + gvk.Group
	req := c.rc.Get().AbsPath(
		"/apis", subGroup, gvk.Version,
		"namespaces", namespace, resource, name, "manifests-with-data-restoration",
	)
	if targetNamespace != "" {
		req = req.Param("targetNamespace", targetNamespace)
	}
	raw, err := req.DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("call domain restore apiserver (%s/%s %s/%s): %w", subGroup, gvk.Version, namespace, name, err)
	}
	objs, err := decodeManifestArray(raw)
	if err != nil {
		return nil, err
	}
	if c.log != nil {
		c.log.Debug("[api] delegated domain subtree restore", "group", subGroup, "resource", resource, "namespace", namespace, "name", name, "objects", len(objs))
	}
	return objs, nil
}

// resourceFor resolves the lowercase plural resource for a domain snapshot GVK via the RESTMapper.
func (c *DomainRestoreClient) resourceFor(gvk schema.GroupVersionKind) (string, error) {
	if c.mapper == nil {
		return "", fmt.Errorf("no REST mapper to resolve resource for %s", gvk.String())
	}
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return "", fmt.Errorf("resolve resource for %s: %w", gvk.String(), err)
	}
	return mapping.Resource.Resource, nil
}

// decodeManifestArray decodes a JSON array of Kubernetes objects into unstructured objects.
func decodeManifestArray(raw []byte) ([]unstructured.Unstructured, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list []unstructured.Unstructured
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode domain restore manifests array: %w", err)
	}
	return list, nil
}
