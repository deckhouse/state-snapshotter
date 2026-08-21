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
	"fmt"
	"net"
	"net/http"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	utilversion "k8s.io/component-base/version"
	openapihandler "k8s.io/kube-openapi/pkg/handler"
	openapihandler3 "k8s.io/kube-openapi/pkg/handler3"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// buildAggregatedAPIServer assembles a k8s.io/apiserver genericapiserver that fronts the existing
// connector-style subresource handlers (delegate) with the standard aggregation-layer handler chain:
//
//   - DelegatingAuthentication: front-proxy requestheader auth (the kube-apiserver proxy client cert is
//     validated against requestheader-client-ca-file + requestheader-allowed-names read from the
//     extension-apiserver-authentication ConfigMap) UNION bearer-token auth via TokenReview. This
//     replaces the previous hand-written mTLS middleware + --api-allowed-client-cns allowlist; the
//     front-proxy mTLS itself is preserved by the requestheader authenticator.
//   - DelegatingAuthorization: every request is authorized via SubjectAccessReview against the resource
//     attributes genericapiserver derives from the URL (verb/group/resource/subresource/namespace), so
//     end-user and inter-server (SA-token) calls are gated by RBAC consistently. Health probes are
//     exempt (AlwaysAllowPaths defaults to /healthz,/readyz,/livez).
//
// The delegate carries all of our existing routes; it is mounted as the genericapiserver
// NonGoRestfulMux NotFoundHandler so every path that genericapiserver does not own itself (our group
// discovery + subresources) is served by it, but still flows through the authn/authz filters.
//
// Authn/authz talk to the kube-apiserver via the pod's in-cluster config (SA token); no kubeconfig flag
// is needed. Serving cert/key are the same files previously consumed directly.
//
// The server also serves /openapi/v2 and /openapi/v3 (see installOpenAPIRoutes) — an aggregated
// apiserver that answers 404 there keeps the kube-apiserver OpenAPI aggregation controllers retrying
// and logging for as long as the APIService exists.
func buildAggregatedAPIServer(name, addr, certFile, keyFile string, delegate http.Handler) (*genericapiserver.GenericAPIServer, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("serving cert and key files are required for the aggregated apiserver")
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("parse api address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("parse api port %q: %w", portStr, err)
	}

	// A minimal scheme is enough: we register no typed REST storage, but genericapiserver needs codecs
	// that can serialize metav1.Status for its error responses.
	scheme := runtime.NewScheme()
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	codecs := serializer.NewCodecFactory(scheme)

	config := genericapiserver.NewConfig(codecs)
	config.EffectiveVersion = utilversion.DefaultBuildEffectiveVersion()

	// WithLoopback is required: genericapiserver.New() rejects a config whose LoopbackClientConfig is nil
	// ("Genericapiserver.New() called with config.LoopbackClientConfig == nil"). Only the loopback variant's
	// ApplyTo generates the self-signed loopback cert and populates config.LoopbackClientConfig; the plain
	// SecureServingOptions.ApplyTo leaves it nil even when our own serving cert/key are provided.
	secure := genericoptions.NewSecureServingOptions().WithLoopback()
	secure.BindPort = port
	if host != "" {
		secure.BindAddress = net.ParseIP(host)
	}
	secure.ServerCert.CertKey.CertFile = certFile
	secure.ServerCert.CertKey.KeyFile = keyFile
	if err := secure.ApplyTo(&config.SecureServing, &config.LoopbackClientConfig); err != nil {
		return nil, fmt.Errorf("apply secure serving: %w", err)
	}

	authn := genericoptions.NewDelegatingAuthenticationOptions()
	if err := authn.ApplyTo(&config.Authentication, config.SecureServing, config.OpenAPIConfig); err != nil {
		return nil, fmt.Errorf("apply delegated authentication: %w", err)
	}

	authz := genericoptions.NewDelegatingAuthorizationOptions()
	if err := authz.ApplyTo(&config.Authorization); err != nil {
		return nil, fmt.Errorf("apply delegated authorization: %w", err)
	}

	return newAggregatedServer(config.Complete(nil), name, delegate)
}

// newAggregatedServer turns a completed config into the running server: the endpoints it owns itself,
// the OpenAPI documents, and the delegate behind everything else. It is split from the config assembly
// above because that half cannot run outside a cluster (delegated authn/authz read the
// extension-apiserver-authentication ConfigMap, secure serving binds a port) — this half is what the
// specs exercise.
func newAggregatedServer(completed genericapiserver.CompletedConfig, name string, delegate http.Handler) (*genericapiserver.GenericAPIServer, error) {
	server, err := completed.New(name, genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, fmt.Errorf("build generic apiserver: %w", err)
	}

	if err := installOpenAPIRoutes(server, name); err != nil {
		return nil, fmt.Errorf("install openapi routes: %w", err)
	}

	// genericapiserver owns its own endpoints: /healthz, /readyz, /livez, /version, /apis discovery and
	// the OpenAPI documents installed just above. Our routes are not registered as typed API groups, so
	// everything else — the group discovery and the subresources — falls through to this delegate.
	server.Handler.NonGoRestfulMux.NotFoundHandler(delegate)

	return server, nil
}

// installOpenAPIRoutes makes the server answer /openapi/v2 and /openapi/v3.
//
// The kube-apiserver OpenAPI aggregation controllers poll both paths for every APIService. Without
// these routes the requests fall through to the delegate mux, which has no such route and answers 404,
// and the control plane then logs "failed to download <apiservice>: resource not found" and requeues
// for as long as the APIService exists. The upstream branch meant to skip a 404 silently cannot fire:
// the sentinel error is wrapped in a message before the caller compares against it. What ends the retry
// loop is the 200 — the content of the document does not matter to it.
//
// The documents are built here rather than through genericapiserver.DefaultOpenAPIConfig on purpose.
// That path builds the spec from the registered go-restful routes and needs a generated definition for
// every model they mention (starting with k8s.io/apimachinery/pkg/version.Info behind /version); with
// no openapi-gen output in this module it does not degrade — it calls klog.Fatalf in PrepareRun and the
// controller never starts.
func installOpenAPIRoutes(server *genericapiserver.GenericAPIServer, name string) error {
	v2 := openapihandler.NewOpenAPIService(&spec.Swagger{
		SwaggerProps: spec.SwaggerProps{
			Swagger: "2.0",
			Info: &spec.Info{
				InfoProps: spec.InfoProps{
					Title: name,
					// Nothing injects a build version into this binary, so anything derived from the
					// build would report the version of the k8s libraries as if it were the module's.
					// This is the same placeholder the upstream config completion falls back to, and
					// the aggregator ignores the Info of a downloaded document anyway.
					Version: "unversioned",
				},
			},
			// Empty, and not a placeholder: the subresources are connector-style routes on the delegate
			// mux (see NewServer), not typed REST storage, so there is no generated schema to publish.
			// Of a downloaded document the aggregator keeps only the paths under this APIService's
			// group and version, so an empty one contributes nothing to the cluster-wide spec.
			Paths: &spec.Paths{Paths: map[string]spec.PathItem{}},
		},
	})
	v2.RegisterOpenAPIVersionedService("/openapi/v2", server.Handler.NonGoRestfulMux)

	// No group version is registered, so the v3 discovery document is an empty path map. That is a
	// valid answer: it tells the aggregator this server publishes no v3 schema, instead of a 404 that
	// it treats as a failed download.
	v3 := openapihandler3.NewOpenAPIService()
	if err := v3.RegisterOpenAPIV3VersionedService("/openapi/v3", server.Handler.NonGoRestfulMux); err != nil {
		return fmt.Errorf("register /openapi/v3: %w", err)
	}

	return nil
}
