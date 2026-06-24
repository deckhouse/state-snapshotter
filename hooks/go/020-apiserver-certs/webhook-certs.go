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

package apiserver_certs

import (
	"fmt"

	tlscertificate "github.com/deckhouse/module-sdk/common-hooks/tls-certificate"

	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
)

// Register hook that generates self-signed TLS certificates for APIService and ValidatingWebhook
// CA sync with front-proxy-ca is handled by 030-front-proxy-ca-sync hook
var _ = tlscertificate.RegisterInternalTLSHookEM(tlscertificate.GenSelfSignedTLSHookConf{
	CommonCACanonicalName: fmt.Sprintf("%s-%s", consts.ModulePluralName, consts.APIServerCertCN),
	CN:                    consts.APIServerCertCN,
	TLSSecretName:         consts.APIServerSecretName,
	Namespace:             consts.ModuleNamespace,
	SANs: tlscertificate.DefaultSANs([]string{
		consts.APIServerCertCN,
		fmt.Sprintf("%s.%s", consts.APIServerCertCN, consts.ModuleNamespace),
		fmt.Sprintf("%s.%s.svc", consts.APIServerCertCN, consts.ModuleNamespace),
		// %CLUSTER_DOMAIN%://  is a special value to generate SAN like 'svc_name.svc_namespace.svc.cluster.local'
		fmt.Sprintf("%%CLUSTER_DOMAIN%%://%s.%s.svc", consts.APIServerCertCN, consts.ModuleNamespace),
		// Also include webhooks service for ValidatingWebhook
		consts.WebhookCertCN,
		fmt.Sprintf("%s.%s", consts.WebhookCertCN, consts.ModuleNamespace),
		fmt.Sprintf("%s.%s.svc", consts.WebhookCertCN, consts.ModuleNamespace),
		fmt.Sprintf("%%CLUSTER_DOMAIN%%://%s.%s.svc", consts.WebhookCertCN, consts.ModuleNamespace),
	}),
	FullValuesPathPrefix: fmt.Sprintf("%s.internal.apiServerCert", consts.ModuleName),
})

// Separate self-signed serving certificate for the DOMAIN controller's aggregated API server. It has its
// own CA so the two pods stay independent (each mounts only its own serving key) and the domain APIService
// registers its own caBundle. The kube-apiserver -> domain mTLS client side uses the k8s-managed
// front-proxy (requestheader) CA, loaded by the domain binary from extension-apiserver-authentication.
var _ = tlscertificate.RegisterInternalTLSHookEM(tlscertificate.GenSelfSignedTLSHookConf{
	CommonCACanonicalName: fmt.Sprintf("%s-%s", consts.ModulePluralName, consts.DomainAPIServerCertCN),
	CN:                    consts.DomainAPIServerCertCN,
	TLSSecretName:         consts.DomainAPIServerSecretName,
	Namespace:             consts.ModuleNamespace,
	SANs: tlscertificate.DefaultSANs([]string{
		consts.DomainAPIServerCertCN,
		fmt.Sprintf("%s.%s", consts.DomainAPIServerCertCN, consts.ModuleNamespace),
		fmt.Sprintf("%s.%s.svc", consts.DomainAPIServerCertCN, consts.ModuleNamespace),
		fmt.Sprintf("%%CLUSTER_DOMAIN%%://%s.%s.svc", consts.DomainAPIServerCertCN, consts.ModuleNamespace),
	}),
	FullValuesPathPrefix: fmt.Sprintf("%s.internal.domainApiServerCert", consts.ModuleName),
})
