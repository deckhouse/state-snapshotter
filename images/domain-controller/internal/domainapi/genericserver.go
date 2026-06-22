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
)

// buildAggregatedAPIServer assembles a k8s.io/apiserver genericapiserver that fronts the domain's
// connector-style restore subresources with the standard aggregation-layer handler chain (front-proxy
// requestheader + TokenReview authentication, SubjectAccessReview authorization). It mirrors the core
// server's builder so both aggregated apiservers enforce the same contract; see the core
// internal/api/genericserver.go for the full rationale.
//
// The delegate carries the domain's existing routes and is mounted as the genericapiserver
// NonGoRestfulMux NotFoundHandler so it stays behind the authn/authz filters. Authn/authz reach the
// kube-apiserver via the pod's in-cluster config (SA token).
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

	completed := config.Complete(nil)
	server, err := completed.New(name, genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, fmt.Errorf("build generic apiserver: %w", err)
	}

	server.Handler.NonGoRestfulMux.NotFoundHandler(delegate)

	return server, nil
}
