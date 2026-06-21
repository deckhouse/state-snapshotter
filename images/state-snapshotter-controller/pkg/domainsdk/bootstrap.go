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

package domainsdk

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	frontProxyCANamespace = "kube-system"
	frontProxyCAConfigMap = "extension-apiserver-authentication"
	frontProxyCAKey       = "requestheader-client-ca-file"
)

// LoadFrontProxyCA reads the k8s-managed front-proxy CA from the extension-apiserver-authentication
// ConfigMap in kube-system. A domain aggregated API server uses it to verify the client certificates
// kube-apiserver presents when proxying aggregation-layer requests — no bespoke PKI is involved. scheme
// must have core/v1 registered. It returns an error if the ConfigMap or the requestheader CA key is
// missing/empty, so callers can fail closed when mTLS is required.
func LoadFrontProxyCA(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) ([]byte, error) {
	directClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create client for front-proxy CA: %w", err)
	}
	return LoadFrontProxyCAFromReader(ctx, directClient)
}

// LoadFrontProxyCAFromReader is the reader-backed core of LoadFrontProxyCA, split out so callers that
// already have a client (and tests with a fake one) can exercise the same fail-closed contract without
// constructing a client from a *rest.Config.
func LoadFrontProxyCAFromReader(ctx context.Context, reader client.Reader) ([]byte, error) {
	cm := &v1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: frontProxyCANamespace, Name: frontProxyCAConfigMap}, cm); err != nil {
		return nil, fmt.Errorf("read %s ConfigMap: %w", frontProxyCAConfigMap, err)
	}
	caData, ok := cm.Data[frontProxyCAKey]
	if !ok || caData == "" {
		return nil, fmt.Errorf("%s not found in %s ConfigMap", frontProxyCAKey, frontProxyCAConfigMap)
	}
	return []byte(caData), nil
}

// ParseAllowedCNs splits a comma-separated list of allowed client-certificate CNs (the front-proxy
// client identities the domain API server accepts), trimming whitespace and dropping empty entries. It
// always returns a non-nil slice.
func ParseAllowedCNs(raw string) []string {
	out := []string{}
	for _, cn := range strings.Split(raw, ",") {
		if cn = strings.TrimSpace(cn); cn != "" {
			out = append(out, cn)
		}
	}
	return out
}
