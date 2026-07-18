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

package tests

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- Publish (ingress + tokens) infrastructure sanity-check (E2E_PUBLISH) --------------------------
//
// DataExport/DataImport publish:true expose a snapshot outside the cluster through an ingress on
// api.<publicDomain>, reusing the TLS secret of the origin `kubernetes-api` Ingress (the user-authn
// publishAPI publication of the kube-API). Every publish prerequisite is already provisioned by the
// storage-e2e bootstrap:
//   - user-authn publishAPI.enabled=true (config.yml.tpl / bootstrap.tpl),
//   - global publicDomainTemplate = %s.<masterIP>.sslip.io (setup.go), a PUBLIC wildcard DNS that
//     resolves api.<masterIP>.sslip.io to the nested master from anywhere,
//   - ingress-nginx (class `nginx`) from the Default bundle.
// So the E2E_PUBLISH step is a BeforeSuite SANITY-CHECK, not an installer: it fails fast with a clear
// message when the installed profile is missing a piece (a broken install, not something a test should
// repair) and records the discovered facts in suitePublishInfra for the publish specs.

const (
	// originIngressName / kubeAPISubdomain mirror storage-foundation's common/consts.go
	// (OriginIngressName = "kubernetes-api") and common/publish/ingress.go (KubeAPISubdomain = "api").
	// The origin Ingress publishes the kube-API and provides the TLS secret the DataExport/DataImport
	// ingresses reuse; the publish host is api.<publicDomain>.
	originIngressName = "kubernetes-api"
	kubeAPISubdomain  = "api"

	// ingressNginxModuleName is the Deckhouse module serving the `nginx` IngressClass. Publish ingresses
	// set spec.ingressClassName=nginx (the data-manager `ingressClassName` env), so this must be Ready.
	ingressNginxModuleName = "ingress-nginx"
	nginxIngressClassName  = "nginx"

	// publishInfraCheckTO bounds the sanity-check. The infra is expected to already be present (the
	// bootstrap provisioned it), so this is a short grace window for a controller that is mid-roll, not
	// a real "wait for install" budget — the check must fail fast on a broken profile.
	publishInfraCheckTO = 3 * time.Minute
)

// originIngressCandidateNamespaces are the version-dependent locations of the origin `kubernetes-api`
// Ingress: DKP dev / >=1.77 place it in kube-system, older releases in d8-user-authn
// (storage-foundation common/consts.go OriginIngressNamespace default). Searched in this order.
var originIngressCandidateNamespaces = []string{"kube-system", "d8-user-authn"}

// publishInfra holds the ingress/publish facts discovered by the E2E_PUBLISH sanity-check. The publish
// specs (helpers, DataExport/DataImport, manifests) read it to build the expected status.publicURL
// prefix and to resolve the nested master IP for the external (base-cluster) curl path.
type publishInfra struct {
	// originIngressNamespace is where the origin `kubernetes-api` Ingress was found.
	originIngressNamespace string
	// originIngressHost is the host on the origin `kubernetes-api` Ingress (api.<publicDomain>). It is
	// the expected prefix of every DataExport/DataImport status.publicURL.
	originIngressHost string
	// publicDomainTemplate is the raw global-ModuleConfig value, e.g. "%s.10.211.1.7.sslip.io".
	publicDomainTemplate string
	// publicDomain is publicDomainTemplate with the leading "%s." trimmed, e.g. "10.211.1.7.sslip.io"
	// (mirrors storage-foundation common/publish/ingress.go).
	publicDomain string
	// masterIP is the nested master IPv4 embedded in the sslip.io domain, used as the --resolve fallback
	// for the external curl path. Empty when the domain is not a parseable sslip.io host.
	masterIP string
}

// suitePublishInfra is populated by checkPublishInfra when E2E_PUBLISH is set; empty otherwise.
var suitePublishInfra publishInfra

// checkPublishInfra is the E2E_PUBLISH BeforeSuite sanity-check. It INSTALLS NOTHING: it only verifies
// the bootstrap-provisioned publish prerequisites are present and records them in suitePublishInfra.
func checkPublishInfra() {
	ctx, cancel := context.WithTimeout(context.Background(), publishInfraCheckTO)
	defer cancel()

	By("E2E_PUBLISH: sanity-checking the publish infrastructure (installs nothing)")

	// (c) publicDomainTemplate must be set on the global ModuleConfig (the bootstrap sets it to
	// %s.<masterIP>.sslip.io). Everything else (the publish host, the --resolve fallback IP) derives
	// from it, so check it first.
	globalMC, err := suiteDyn.Resource(moduleConfigGVR).Get(ctx, "global", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(),
		"E2E_PUBLISH: read the global ModuleConfig (publish needs spec.settings.modules.publicDomainTemplate)")
	// TODO(e2e-publish): verify on cluster — confirm the publicDomainTemplate path is
	// spec.settings.modules.publicDomainTemplate on the ModuleConfig named "global" (matches the
	// storage-e2e bootstrap config.yml.tpl; the settings schema version is 2).
	tmpl, found, err := unstructured.NestedString(globalMC.Object, "spec", "settings", "modules", "publicDomainTemplate")
	Expect(err).NotTo(HaveOccurred(),
		"E2E_PUBLISH: read spec.settings.modules.publicDomainTemplate from the global ModuleConfig")
	tmpl = strings.TrimSpace(tmpl)
	Expect(found && tmpl != "").To(BeTrue(),
		"E2E_PUBLISH: global ModuleConfig spec.settings.modules.publicDomainTemplate is empty; the storage-e2e bootstrap sets it to %s.<masterIP>.sslip.io — the installation profile is broken")
	suitePublishInfra.publicDomainTemplate = tmpl
	suitePublishInfra.publicDomain = strings.TrimPrefix(tmpl, "%s.")
	suitePublishInfra.masterIP = masterIPFromSslipDomain(suitePublishInfra.publicDomain)

	// (a) The origin `kubernetes-api` Ingress must exist. Search the version-dependent namespaces and
	// record the host it publishes as the expected publicURL prefix.
	var (
		originIng *networkingv1.Ingress
		getErrs   []string
	)
	for _, ns := range originIngressCandidateNamespaces {
		ing, gerr := suiteClientset.NetworkingV1().Ingresses(ns).Get(ctx, originIngressName, metav1.GetOptions{})
		if gerr == nil {
			originIng = ing
			suitePublishInfra.originIngressNamespace = ns
			break
		}
		if !apierrors.IsNotFound(gerr) {
			getErrs = append(getErrs, fmt.Sprintf("%s/%s: %v", ns, originIngressName, gerr))
		}
	}
	// TODO(e2e-publish): verify on cluster — confirm the origin Ingress is named "kubernetes-api" and
	// lives in one of {kube-system, d8-user-authn} for the profile under test; extend the candidate list
	// if a newer/older DKP places it elsewhere.
	Expect(originIng).NotTo(BeNil(), fmt.Sprintf(
		"E2E_PUBLISH: origin Ingress %q not found in any of %v (it publishes the kube-API via user-authn publishAPI and provides the TLS secret DataExport/DataImport ingresses reuse). Non-NotFound errors: %v",
		originIngressName, originIngressCandidateNamespaces, getErrs))

	expectedHost := fmt.Sprintf("%s.%s", kubeAPISubdomain, suitePublishInfra.publicDomain)
	host := originIngressHost(originIng, expectedHost)
	Expect(host).NotTo(BeEmpty(), fmt.Sprintf(
		"E2E_PUBLISH: origin Ingress %s/%s has no host rule; cannot derive the publicURL prefix",
		suitePublishInfra.originIngressNamespace, originIngressName))
	if host != expectedHost {
		// Not fatal: the Ingress host is authoritative for the publicURL. A mismatch usually means the
		// bootstrap publicDomainTemplate and the actual publish domain diverged; log it and continue.
		GinkgoWriter.Printf("  E2E_PUBLISH: WARNING origin Ingress host %q != host derived from publicDomainTemplate %q; using the Ingress host as authoritative\n", host, expectedHost)
	}
	suitePublishInfra.originIngressHost = host

	// (b) ingress-nginx must be Ready and serve the `nginx` IngressClass.
	_, err = suiteClientset.NetworkingV1().IngressClasses().Get(ctx, nginxIngressClassName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf(
		"E2E_PUBLISH: IngressClass %q not found; publish ingresses set spec.ingressClassName=%s (the data-manager ingressClassName env), so ingress-nginx must serve that class",
		nginxIngressClassName, nginxIngressClassName))

	By("E2E_PUBLISH: waiting for the ingress-nginx module to be Ready")
	// TODO(e2e-publish): verify on cluster — confirm the Module CR is named "ingress-nginx" and reaches
	// phase Ready in this profile (storagekube.WaitForModuleReady queries modules.deckhouse.io). If the
	// controller readiness is better expressed via the IngressNginxController CR / controller pods on this
	// DKP, swap this for that check.
	Expect(storagekube.WaitForModuleReady(ctx, suiteRestCfg, ingressNginxModuleName, publishInfraCheckTO)).To(
		Succeed(),
		"E2E_PUBLISH: ingress-nginx module is not Ready; publish needs the ingress controller to serve api.<publicDomain>")

	GinkgoWriter.Printf("E2E_PUBLISH sanity-check OK:\n")
	GinkgoWriter.Printf("  origin ingress:            %s/%s (host %s)\n", suitePublishInfra.originIngressNamespace, originIngressName, suitePublishInfra.originIngressHost)
	GinkgoWriter.Printf("  publicDomainTemplate:      %s\n", suitePublishInfra.publicDomainTemplate)
	GinkgoWriter.Printf("  publicDomain:              %s\n", suitePublishInfra.publicDomain)
	GinkgoWriter.Printf("  master IP (sslip):         %q\n", suitePublishInfra.masterIP)
	GinkgoWriter.Printf("  expected publicURL prefix: https://%s/<ns>/<kind>/<name>/\n", suitePublishInfra.originIngressHost)
}

// originIngressHost returns the host the origin Ingress publishes. It prefers the rule whose host
// equals expected (api.<publicDomain>) and otherwise falls back to the first non-empty host rule.
func originIngressHost(ing *networkingv1.Ingress, expected string) string {
	var first string
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			continue
		}
		if rule.Host == expected {
			return rule.Host
		}
		if first == "" {
			first = rule.Host
		}
	}
	return first
}

// masterIPFromSslipDomain extracts the embedded IPv4 from a dot-form sslip.io wildcard domain
// (e.g. "10.211.1.7.sslip.io" -> "10.211.1.7"). Best-effort: returns "" for a non-sslip or
// unparseable domain, in which case the external curl path relies on public DNS instead of the
// --resolve fallback.
func masterIPFromSslipDomain(domain string) string {
	const suffix = ".sslip.io"
	if !strings.HasSuffix(domain, suffix) {
		return ""
	}
	ipPart := strings.TrimSuffix(domain, suffix)
	if net.ParseIP(ipPart) == nil {
		return ""
	}
	return ipPart
}
