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
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- Publish (ingress + tokens) infrastructure step (E2E_PUBLISH) ----------------------------------
//
// DataExport/DataImport publish:true expose a snapshot outside the cluster through an ingress on
// api.<publicDomain>, reusing the TLS secret of the origin `kubernetes-api` Ingress (the user-authn
// publishAPI publication of the kube-API). The publish prerequisites are:
//   - global publicDomainTemplate = %s.<masterIP>.sslip.io (a PUBLIC wildcard DNS that resolves
//     api.<masterIP>.sslip.io to the nested master from anywhere),
//   - user-authn publishAPI.enabled=true (publishes the kube-API and provides the reused TLS secret),
//   - a working `nginx` IngressClass (ingress-nginx module + an IngressNginxController).
//
// The storage-e2e bootstrap wires only the first two (config.yml.tpl / setup.go) — and ONLY on
// alwaysCreateNew, when it installs a fresh DKP. On alwaysUseExisting (or commander) that bootstrap config
// is NOT applied. This step therefore splits the prerequisites by whether a test can safely produce them:
//   - publicDomainTemplate and the origin `kubernetes-api` Ingress are ASSERTED (fail-fast): the former is
//     cluster-global (it rewrites every module's public domain) and the latter follows from user-authn
//     publishAPI — neither is something a test should fabricate on a cluster it may not own.
//   - the `nginx` IngressClass is ENSURED by ensureIngress: reused if present (no mutation), else
//     provisioned (enable ingress-nginx + a HostPort IngressNginxController on the master). The bootstrap
//     configures no ingress at all, so this is what makes publish work on alwaysUseExisting (and on a
//     fresh cluster). Operators who do not want the suite to touch ingress set E2E_PUBLISH=false.
//
// It runs BEFORE the module-readiness wait (fail fast on a cluster that cannot support publish, without
// first burning minutes converging the snapshot stack) and records the resulting facts in suitePublishInfra
// for the publish specs.

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

	// publishInfraCheckTO bounds the read-only sanity checks (publicDomainTemplate, origin ingress). The
	// facts are expected to already be present, so this is a short grace window, not a "wait for install"
	// budget. ensureIngress uses the larger suiteCfg.moduleReadyTO when it has to provision ingress-nginx.
	publishInfraCheckTO = 3 * time.Minute

	// ingressNginxCRDName is the IngressNginxController CRD; it must be Established (it ships with the
	// ingress-nginx module) before ensureIngress can create the controller CR.
	ingressNginxCRDName = "ingressnginxcontrollers.deckhouse.io"

	// publishIngressControllerName is the IngressNginxController ensureIngress creates when a cluster has
	// no working ingress class. Kept short: the module derives child resource names from it.
	publishIngressControllerName = "nginx"

	// defaultPublishIngressInlet is the inlet ensureIngress uses by default. HostPort is the only inlet
	// that works on the storage-e2e static nested cluster, whose publish domain is %s.<masterIP>.sslip.io:
	// the controller runs on the master (see the all-taints toleration below) and HostPort exposes it on
	// that master IP:443. Override with E2E_PUBLISH_INGRESS_INLET for a different environment (e.g. a
	// LoadBalancer cluster), or set E2E_PUBLISH=false to skip publish (and ingress setup) entirely.
	defaultPublishIngressInlet = "HostPort"
)

// envPublishIngressInlet overrides the inlet of the IngressNginxController ensureIngress provisions.
const envPublishIngressInlet = "E2E_PUBLISH_INGRESS_INLET"

// ingressNginxControllerGVR is the cluster-scoped IngressNginxController (deckhouse.io/v1) that turns the
// ingress-nginx module into an actually-serving IngressClass. The storage-e2e bootstrap never creates one
// (it configures neither the module nor a controller), so ensureIngress provisions it on clusters that
// lack a working `nginx` class — chiefly alwaysUseExisting.
var ingressNginxControllerGVR = schema.GroupVersionResource{
	Group: "deckhouse.io", Version: "v1", Resource: "ingressnginxcontrollers",
}

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
	// ingressClass is the IngressClass the publish ingresses bind to, resolved (and, on a cluster without
	// one, provisioned) by ensureIngress. Recorded for diagnostics; the data-manager default is `nginx`.
	ingressClass string
	// originTLSSecretName is the TLS secret the origin `kubernetes-api` Ingress references for
	// originIngressHost. The DataExport/DataImport controllers reuse it — they copy it into each publish
	// namespace (storage-foundation common/publish/ingress.go ensureIngressSecret) — so it must exist and
	// be issued. On a private %s.<masterIP>.sslip.io domain that requires https.mode=CertManager with a
	// selfsigned clusterIssuer: the default letsencrypt issuer cannot solve an ACME challenge for a
	// non-public IP, so the secret never appears and publish fails with "failed to get origin secret".
	originTLSSecretName string
}

// suitePublishInfra is populated by checkPublishInfra when E2E_PUBLISH is set; empty otherwise.
var suitePublishInfra publishInfra

// checkPublishInfra is the E2E_PUBLISH BeforeSuite step, run BEFORE the module-readiness wait so a cluster
// that cannot support publish fails fast. It ASSERTS the two prerequisites a test must not fabricate — the
// cluster-global publicDomainTemplate (checked first) and the origin `kubernetes-api` Ingress (needs
// user-authn publishAPI) — and ENSURES the one it safely can: a working ingress class (ensureIngress reuses
// an existing one or provisions the ingress-nginx module + a HostPort controller on a cluster that lacks
// one). Discovered/So-provisioned facts are recorded in suitePublishInfra for the publish specs.
func checkPublishInfra() {
	// Budget for the two potentially-slow steps that share this context: waiting for cert-manager to issue
	// the origin TLS secret (publishInfraCheckTO) and ensureIngress possibly provisioning ingress-nginx
	// from scratch (module roll + controller pods, up to moduleReadyTO). The read-only asserts are quick.
	ctx, cancel := context.WithTimeout(context.Background(), 2*publishInfraCheckTO+suiteCfg.moduleReadyTO)
	defer cancel()

	By("E2E_PUBLISH: checking the publish infrastructure (asserts domain + origin ingress; ensures ingress class)")

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

	// (b) The origin Ingress TLS secret must exist and be issued: DataExport/DataImport reuse it (copy it
	// into each publish namespace), so an absent/unissued secret makes every publish fail deep inside the
	// data-manager reconcile with a cryptic "failed to get origin secret". Resolve its name from the origin
	// Ingress and wait for cert-manager here, turning that into an actionable fail-fast. It is independent
	// of the ingress controller (a selfsigned issuer needs no HTTP-01), so it is checked before ensureIngress.
	secretName := originIngressTLSSecretName(originIng, host)
	Expect(secretName).NotTo(BeEmpty(), fmt.Sprintf(
		"E2E_PUBLISH: origin Ingress %s/%s declares no TLS secret for host %s — publish has no TLS secret to reuse",
		suitePublishInfra.originIngressNamespace, originIngressName, host))
	suitePublishInfra.originTLSSecretName = secretName
	waitOriginTLSSecret(ctx, suitePublishInfra.originIngressNamespace, secretName, publishInfraCheckTO)

	// (d) A working ingress class the publish ingresses (spec.ingressClassName=nginx, the data-manager
	// default) can bind to. ensureIngress reuses an existing class, or provisions the ingress-nginx module
	// + a HostPort controller on a cluster that lacks one (typically alwaysUseExisting).
	suitePublishInfra.ingressClass = ensureIngress(ctx)

	GinkgoWriter.Printf("E2E_PUBLISH check OK:\n")
	GinkgoWriter.Printf("  ingress class:             %s\n", suitePublishInfra.ingressClass)
	GinkgoWriter.Printf("  origin ingress:            %s/%s (host %s)\n", suitePublishInfra.originIngressNamespace, originIngressName, suitePublishInfra.originIngressHost)
	GinkgoWriter.Printf("  origin TLS secret:         %s/%s\n", suitePublishInfra.originIngressNamespace, suitePublishInfra.originTLSSecretName)
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

// originIngressTLSSecretName returns the TLS secret the origin Ingress uses for host. It prefers the TLS
// entry that lists host (api.<publicDomain>) and otherwise falls back to the first entry with a non-empty
// SecretName. Returns "" when the Ingress declares no usable TLS secret.
func originIngressTLSSecretName(ing *networkingv1.Ingress, host string) string {
	var first string
	for _, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			continue
		}
		for _, h := range tls.Hosts {
			if h == host {
				return tls.SecretName
			}
		}
		if first == "" {
			first = tls.SecretName
		}
	}
	return first
}

// waitOriginTLSSecret blocks until the origin TLS secret exists and carries a non-empty tls.crt (the signal
// cert-manager finished issuing it), or fails after timeout. The failure message names the usual cause on a
// storage-e2e cluster: the publish domain is a private %s.<masterIP>.sslip.io, so the default letsencrypt
// issuer cannot solve an ACME challenge for it and the secret never appears — the cluster needs
// https.mode=CertManager with a selfsigned clusterIssuer (the alwaysCreateNew bootstrap should set this),
// or E2E_PUBLISH=false to skip publish entirely.
func waitOriginTLSSecret(ctx context.Context, namespace, name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		sec, err := suiteClientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil && len(sec.Data["tls.crt"]) > 0:
			return
		case err == nil:
			last = fmt.Sprintf("secret %s/%s exists but its tls.crt is empty (cert-manager has not issued it yet)", namespace, name)
		default:
			last = err.Error()
		}
		if time.Now().After(deadline) {
			Fail(fmt.Sprintf(
				"E2E_PUBLISH: origin TLS secret %s/%s was not issued within %s. DataExport/DataImport reuse it for the publish ingress, so publish cannot work without it. On a private *.sslip.io domain the default letsencrypt issuer cannot validate the domain — set the global https.mode=CertManager with clusterIssuerName=selfsigned (the storage-e2e alwaysCreateNew bootstrap should configure this), or set E2E_PUBLISH=false to skip publish. Last: %s",
				namespace, name, timeout, last))
		}
		if !sleepCtx(ctx, pollInterval) {
			Fail(fmt.Sprintf("E2E_PUBLISH: context cancelled waiting for origin TLS secret %s/%s: %v", namespace, name, ctx.Err()))
		}
	}
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

// ensureIngress makes a working ingress class available for the publish specs and returns its name. It is
// idempotent and does the minimum a cluster needs:
//   - Fast path: if the `nginx` IngressClass already exists (a cluster that already runs an ingress
//     controller — including any set up out-of-band), it only waits for the ingress-nginx module to be
//     Ready and returns, mutating nothing.
//   - Provision path (typically alwaysUseExisting on a cluster without ingress, but also a fresh
//     alwaysCreateNew, since the storage-e2e bootstrap configures no ingress): it enables the ingress-nginx
//     module, waits for the IngressNginxController CRD, creates a HostPort controller that tolerates every
//     taint (so it also lands on the master, whose IP the publish domain %s.<masterIP>.sslip.io resolves
//     to), then waits for the module to be Ready and the class to appear.
//
// Operators who do not want the suite to touch ingress set E2E_PUBLISH=false, which skips the publish specs
// and this whole step. The class name is `nginx` (the data-manager `ingressClassName` default the publish
// ingresses bind to); it is returned so callers/diagnostics do not hardcode it.
func ensureIngress(ctx context.Context) string {
	class := nginxIngressClassName

	// Fast path: a working class already exists — do not mutate the cluster.
	_, err := suiteClientset.NetworkingV1().IngressClasses().Get(ctx, class, metav1.GetOptions{})
	if err == nil {
		By(fmt.Sprintf("E2E_PUBLISH: IngressClass %q already present — reusing it", class))
		Expect(storagekube.WaitForModuleReady(ctx, suiteRestCfg, ingressNginxModuleName, publishInfraCheckTO)).To(
			Succeed(), "E2E_PUBLISH: ingress-nginx module is not Ready")
		return class
	}
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), fmt.Sprintf("E2E_PUBLISH: get IngressClass %q", class))

	inlet := strings.TrimSpace(os.Getenv(envPublishIngressInlet))
	if inlet == "" {
		inlet = defaultPublishIngressInlet
	}
	By(fmt.Sprintf("E2E_PUBLISH: no %q IngressClass — provisioning ingress-nginx (module + %s controller)", class, inlet))

	// 1. Enable the ingress-nginx module (idempotent; preserves any existing settings).
	ensureIngressNginxModuleEnabled(ctx)

	// 2. The IngressNginxController CRD ships with the module — wait until it is Established before creating
	//    the controller CR.
	Expect(waitObjectCondition(ctx, crdGVR, "", ingressNginxCRDName, "Established", "True", suiteCfg.moduleReadyTO)).To(
		Succeed(), "E2E_PUBLISH: IngressNginxController CRD not Established after enabling ingress-nginx")

	// 3. Create the controller if absent.
	ensureIngressNginxController(ctx, class, inlet)

	// 4. Wait for the module to be Ready and the class to actually materialize (module Ready alone does not
	//    guarantee a serving controller — the IngressClass is the real signal).
	Expect(storagekube.WaitForModuleReady(ctx, suiteRestCfg, ingressNginxModuleName, suiteCfg.moduleReadyTO)).To(
		Succeed(), "E2E_PUBLISH: ingress-nginx module did not become Ready after provisioning")
	waitIngressClassPresent(ctx, class, suiteCfg.moduleReadyTO)

	By(fmt.Sprintf("E2E_PUBLISH: ingress class %q is ready (inlet %s)", class, inlet))
	return class
}

// ensureIngressNginxModuleEnabled enables the ingress-nginx module via its ModuleConfig. It is idempotent:
// it creates the ModuleConfig (version 1, enabled) when absent and patches only spec.enabled=true when a
// config exists but is disabled, never touching existing settings.
func ensureIngressNginxModuleEnabled(ctx context.Context) {
	mc, err := suiteDyn.Resource(moduleConfigGVR).Get(ctx, ingressNginxModuleName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "deckhouse.io/v1alpha1",
			"kind":       "ModuleConfig",
			"metadata":   map[string]interface{}{"name": ingressNginxModuleName},
			"spec": map[string]interface{}{
				"version": int64(1),
				"enabled": true,
			},
		}}
		_, cerr := suiteDyn.Resource(moduleConfigGVR).Create(ctx, obj, metav1.CreateOptions{})
		Expect(cerr).NotTo(HaveOccurred(), "E2E_PUBLISH: create ingress-nginx ModuleConfig (enable)")
		return
	}
	Expect(err).NotTo(HaveOccurred(), "E2E_PUBLISH: get ingress-nginx ModuleConfig")
	if enabled, _, _ := unstructured.NestedBool(mc.Object, "spec", "enabled"); enabled {
		return
	}
	_, perr := suiteDyn.Resource(moduleConfigGVR).Patch(ctx, ingressNginxModuleName, types.MergePatchType,
		[]byte(`{"spec":{"enabled":true}}`), metav1.PatchOptions{})
	Expect(perr).NotTo(HaveOccurred(), "E2E_PUBLISH: enable ingress-nginx via ModuleConfig patch")
}

// ensureIngressNginxController creates the publish IngressNginxController if it does not already exist. The
// controller serves `class` via the given inlet; for HostPort inlets it binds :80/:443 on the nodes and
// tolerates every taint so it also runs on the master (whose IP the publish sslip.io domain resolves to).
func ensureIngressNginxController(ctx context.Context, class, inlet string) {
	spec := map[string]interface{}{
		"ingressClass": class,
		"inlet":        inlet,
		// Tolerate every taint so the HostPort DaemonSet also lands on the master node; the storage-e2e
		// publicDomainTemplate is %s.<masterIP>.sslip.io, so the controller must be reachable on the master.
		"tolerations": []interface{}{
			map[string]interface{}{"operator": "Exists"},
		},
	}
	// HostPort / HostPortWithProxyProtocol require their port block; other inlets (e.g. LoadBalancer) take
	// module defaults.
	switch inlet {
	case "HostPort":
		spec["hostPort"] = map[string]interface{}{"httpPort": int64(80), "httpsPort": int64(443)}
	case "HostPortWithProxyProtocol":
		spec["hostPortWithProxyProtocol"] = map[string]interface{}{"httpPort": int64(80), "httpsPort": int64(443)}
	}
	inc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "deckhouse.io/v1",
		"kind":       "IngressNginxController",
		"metadata":   map[string]interface{}{"name": publishIngressControllerName},
		"spec":       spec,
	}}
	_, err := suiteDyn.Resource(ingressNginxControllerGVR).Create(ctx, inc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "E2E_PUBLISH: create IngressNginxController")
	}
}

// waitIngressClassPresent blocks until the named IngressClass exists (the signal that the provisioned
// ingress-nginx controller is actually serving) or fails after timeout.
func waitIngressClassPresent(ctx context.Context, class string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		_, err := suiteClientset.NetworkingV1().IngressClasses().Get(ctx, class, metav1.GetOptions{})
		if err == nil {
			return
		}
		last = err.Error()
		if time.Now().After(deadline) {
			Fail(fmt.Sprintf("E2E_PUBLISH: IngressClass %q did not appear within %s after provisioning ingress-nginx; last: %s", class, timeout, last))
		}
		if !sleepCtx(ctx, pollInterval) {
			Fail(fmt.Sprintf("E2E_PUBLISH: context cancelled waiting for IngressClass %q: %v", class, ctx.Err()))
		}
	}
}
