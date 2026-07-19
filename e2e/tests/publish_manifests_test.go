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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// --- Published aggregated manifests (no separate APIService ingress) spec (E2E_PUBLISH) ------------
//
// This spec closes the user's open question: the aggregated manifests API is reachable from OUTSIDE the
// nested cluster through the SAME origin `kubernetes-api` ingress host (api.<publicDomain>) that publishes
// the kube-API — no separate APIService ingress is needed. The aggregated apiserver is registered as an
// APIService in the kube-apiserver (aggregation layer), and the kube-apiserver itself is what the
// user-authn publishAPI ingress exposes, so a request to
//   https://api.<domain>/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/<ns>/snapshots/<name>/manifests-download
// flows nginx -> published kube-apiserver -> aggregated apiserver exactly as the in-cluster read does.
//
// It builds a self-contained manifest-only snapshot tree (the same ConfigMap + DemoVirtualMachine source
// the shared capture flow uses — the phase-1&2 `captured` namespace is reaped before the publish specs
// run, so this cannot reuse it), then proves:
//   (a) INTERNAL baseline: the aggregated manifests-download is served in-cluster and matches the live
//       objects (phase-4 raw comparator assertRawManifestsMatchLive).
//   (b) NEGATIVE: the EXTERNAL request with a valid SA Bearer token but NO manifests-download RoleBinding
//       is Forbidden (403) — the aggregated apiserver's delegated SubjectAccessReview denies it.
//   (c) EXTERNAL positive: once the RoleBinding is granted, the external response is 200, is the same set
//       as the internal aggregated response, AND matches the live objects. That equality is the proof the
//       published kube-API ingress carries the aggregated API with no separate ingress.
//
// Authorization: the aggregated apiserver runs DelegatingAuthorization (internal/api/genericserver.go),
// so every request is gated by an SAR against the URL-derived attributes. For this GET the attributes are
// apiGroup=subresources.state-snapshotter.deckhouse.io, resource=snapshots,
// subresource=manifests-download, verb=get — exactly what createManifestsDownloadRole grants (confirmed
// against internal/api/archive_handler.go discovery registration "snapshots/manifests-download").
//
// It reuses publish_helpers_test.go (auth: createServiceAccountIfNotExists / createManifestsDownloadRole /
// bindRoleToServiceAccount / issueServiceAccountToken; the base-cluster curl runner ensureExternalCurlPod /
// externalCurlTarget / runCurlRequest; the external URL helper externalManifestsURL; publishMasterIP for
// the --resolve fallback), the shared capture source (buildManifestOnlySource / createRootSnapshot), the
// aggregated URL + decode helpers (coreSnapshotSubPath / aggGet / decodeManifestArray / findManifest), and
// the phase-4 comparator (assertRawManifestsMatchLive / canonicalManifestChecksum).

const (
	// publishManifestsSnapshot is the root Snapshot of the self-contained manifest-only tree this spec
	// captures and then serves manifests-download for (internally and externally).
	publishManifestsSnapshot = "pub-man-root"

	// publishManifestsSA / publishManifestsRole / publishManifestsBinding are the token identity granted
	// snapshots/manifests-download in the source namespace. Role and RoleBinding are split so the
	// 403-before-binding negative can probe the token while only the SA + Role exist, then bind and re-probe
	// for 200.
	publishManifestsSA      = publishClientSA // "publish-client" (from publish_helpers)
	publishManifestsRole    = "publish-manifests-download"
	publishManifestsBinding = "publish-manifests-download"
)

// publishManifestsSpecs registers the published-manifests flow (env-gated by E2E_PUBLISH). Ordered: the
// source tree is captured once, then the internal baseline, the external negative, and the external
// positive run in sequence (the negative mints the token the positive reuses after binding). Registered
// after the DataExport/DataImport publish specs in the single root Ordered container.
func publishManifestsSpecs() {
	Context("Publish 3: aggregated manifests via the published kube-API (no separate APIService ingress)", Ordered, func() {
		var (
			srcNS        string
			masterIP     string
			token        string
			internalObjs []unstructured.Unstructured
			intPath      string
			extURL       string
		)

		BeforeAll(func() {
			if !suiteCfg.publish {
				Skip("E2E_PUBLISH=false: skipping the published-manifests (aggregated API via ingress) flow (it runs by default)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.captureReadyTO+15*time.Minute)
			defer cancel()

			srcNS = uniqueNS("pub-man")
			masterIP = publishMasterIP(ctx)
			intPath = coreSnapshotSubPath(srcNS, publishManifestsSnapshot, subManifestsDownload)
			extURL = externalManifestsURL(srcNS, publishManifestsSnapshot)

			By("Creating the source namespace " + srcNS)
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			// deleteNamespace honors E2E_KEEP_CLUSTER*; deleting the namespace reaps the source objects, the
			// root Snapshot, and the SA/Role/RoleBinding created in it. It is registered here (container-scoped)
			// so it fires once at container teardown, never as an AfterEach after an It.
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Applying the manifest-only source (ConfigMap + DemoVirtualMachine)")
			Expect(applyObjects(ctx, buildManifestOnlySource(srcNS), srcNS)).To(Succeed())

			By("Creating the root Snapshot " + publishManifestsSnapshot)
			Expect(createRootSnapshot(ctx, srcNS, publishManifestsSnapshot)).To(Succeed())

			By("Waiting for the root Snapshot to become Ready")
			rootContent, err := waitSnapshotReady(ctx, srcNS, publishManifestsSnapshot, suiteCfg.captureReadyTO)
			Expect(err).NotTo(HaveOccurred(), "root Snapshot %s Ready", publishManifestsSnapshot)
			Expect(rootContent).NotTo(BeEmpty(), "the Ready root Snapshot must have a bound SnapshotContent")

			By("Ensuring the base-cluster external curl runner pod")
			Expect(ensureExternalCurlPod(ctx)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer ccancel()
				deleteExternalCurlPod(cctx)
			})
		})

		It("(a) serves the root Snapshot manifests-download INTERNALLY and it matches the live objects", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.captureReadyTO+time.Minute)
			defer cancel()

			By("Reading the aggregated manifests-download from inside the cluster (admin REST client)")
			// The root own-manifests are served only after main latches commonController.manifestCaptured, so
			// on a just-Ready tree a one-shot read can race that latch. Poll until it decodes and carries the
			// source ConfigMap, then record the reference set for the external equality check in (c).
			Eventually(func(g Gomega) {
				body, err := aggGet(ctx, intPath, nil)
				g.Expect(err).NotTo(HaveOccurred(), "GET %s", intPath)
				objs, derr := decodeManifestArray(body)
				g.Expect(derr).NotTo(HaveOccurred())
				_, found := findManifest(objs, "ConfigMap", srcConfigMapName)
				g.Expect(found).To(BeTrue(), "internal manifests-download must contain the source ConfigMap %s", srcConfigMapName)
				internalObjs = objs
			}).WithContext(ctx).WithTimeout(suiteCfg.captureReadyTO).WithPolling(pollInterval).Should(Succeed())

			By("Asserting the internal manifests-download matches the live cluster objects (phase-4 comparator)")
			Expect(assertRawManifestsMatchLive(ctx, srcNS, internalObjs)).To(Succeed())
		})

		It("(b) NEGATIVE: the external manifests-download is Forbidden (403) for a token without the manifests-download RBAC", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			extTarget, terr := externalCurlTarget()
			Expect(terr).NotTo(HaveOccurred())

			By("Creating the token identity (SA + snapshots/manifests-download Role) WITHOUT the RoleBinding yet")
			Expect(createServiceAccountIfNotExists(ctx, srcNS, publishManifestsSA)).To(Succeed())
			Expect(createManifestsDownloadRole(ctx, srcNS, publishManifestsRole)).To(Succeed())
			t, err := issueServiceAccountToken(ctx, srcNS, publishManifestsSA, publishTokenTTL)
			Expect(err).NotTo(HaveOccurred())
			token = t

			By("Requesting the external aggregated manifests-download with the unbound token -> 403")
			// The kube-API is published through the SAME origin `kubernetes-api` ingress host; the request is
			// authenticated (the SA token is a valid kube-apiserver identity, forwarded to the aggregated
			// apiserver via requestheader) but the delegated SAR denies the read with no RoleBinding -> 403 (a
			// valid-token authorization failure, distinct from a 401 authentication failure). External TLS is
			// terminated at nginx with the origin self-signed cert, so runCurlRequest uses -k (no caFile);
			// masterIP enables the sslip.io --resolve fallback when the base pod cannot resolve api.<domain>.
			// TODO(e2e-publish): verify on cluster — confirm the aggregated apiserver's delegated authn accepts
			// the default-audience SA token minted via TokenRequest through the published kube-apiserver
			// (requestheader path); a 401 here would mean a bound-audience token is required (set Audiences in
			// issueServiceAccountToken).
			code, _, cerr := runCurlRequest(ctx, extTarget, extURL, curlRequest{
				token:    token,
				masterIP: masterIP,
				headers:  map[string]string{"Accept": "application/json"},
			})
			Expect(cerr).NotTo(HaveOccurred())
			Expect(code).To(Equal(403), "external manifests-download without the manifests-download RoleBinding must be Forbidden")
		})

		It("(c) EXTERNAL manifests-download through the published kube-API equals INTERNAL and matches live (no separate APIService ingress)", func() {
			Expect(token).NotTo(BeEmpty(), "the negative step must have minted the token")
			Expect(internalObjs).NotTo(BeEmpty(), "the internal baseline step must have captured the reference manifests")

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			extTarget, terr := externalCurlTarget()
			Expect(terr).NotTo(HaveOccurred())

			By("Granting the manifests-download RoleBinding, then reading the external manifests-download (200)")
			Expect(bindRoleToServiceAccount(ctx, srcNS, publishManifestsBinding, publishManifestsRole, srcNS, publishManifestsSA)).To(Succeed())

			// The aggregated apiserver authorizes each request via a fresh SAR (delegated authz), so the SAME
			// token now passes without re-issuing it; the SAR is eventually consistent after the RoleBinding is
			// created, so poll until the external read is authorized.
			var extBody string
			Eventually(func() (int, error) {
				code, body, e := runCurlRequest(ctx, extTarget, extURL, curlRequest{
					token:    token,
					masterIP: masterIP,
					headers:  map[string]string{"Accept": "application/json"},
				})
				extBody = body
				return code, e
			}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(pollInterval).Should(Equal(200),
				"external manifests-download through the published kube-API ingress must succeed once the RoleBinding is in place")

			By("Decoding the external response and asserting it equals the internal aggregated response")
			// TODO(e2e-publish): verify on cluster — the external curl sends Accept: application/json to match
			// the in-cluster REST client; confirm the aggregated apiserver serves the same JSON array body on
			// both paths (the delegate writes JSON directly, so content negotiation should not diverge).
			extObjs, derr := decodeManifestArray([]byte(extBody))
			Expect(derr).NotTo(HaveOccurred(), "decode external manifests-download body")
			Expect(assertManifestSetsEqual(internalObjs, extObjs)).To(Succeed(),
				"the external (ingress) manifests-download must carry the same object set as the internal aggregated response")

			By("Asserting the external manifests-download also matches the live cluster objects (phase-4 comparator)")
			Expect(assertRawManifestsMatchLive(ctx, srcNS, extObjs)).To(Succeed())

			By("Conclusion: the aggregated API is reachable externally through the SAME origin kubernetes-api ingress host — no separate APIService ingress is needed")
			GinkgoWriter.Printf("  external manifests-download URL: %s (origin ingress host %s)\n", extURL, suitePublishInfra.originIngressHost)
		})
	})
}

// --- spec-local helpers ----------------------------------------------------

// manifestChecksumByKey indexes decoded manifest objects by "kind|namespace|name" -> canonical content
// checksum, so two manifests-download responses can be compared as sets independent of array ordering.
// It reuses the phase-4 canonicalManifestChecksum (deterministic JSON marshal + sha256) over the raw
// object (no volatile-metadata stripping): both responses come from the SAME aggregated endpoint reading
// the SAME frozen ManifestCheckpoint, so they must be byte-identical, and a raw compare is the stronger
// assertion. (The separate live comparison uses assertRawManifestsMatchLive, which strips volatile fields.)
func manifestChecksumByKey(objs []unstructured.Unstructured) (map[string]string, error) {
	out := make(map[string]string, len(objs))
	for i := range objs {
		obj := &objs[i]
		key := obj.GetKind() + "|" + obj.GetNamespace() + "|" + obj.GetName()
		sum, _, err := canonicalManifestChecksum(obj.Object)
		if err != nil {
			return nil, fmt.Errorf("checksum manifest %s: %w", key, err)
		}
		out[key] = sum
	}
	return out, nil
}

// assertManifestSetsEqual verifies two decoded manifests-download responses carry the same objects with
// byte-identical content (keyed by kind|namespace|name). Used to prove the EXTERNAL (through the ingress)
// aggregated response equals the INTERNAL (in-cluster) one — i.e. the published kube-API ingress serves
// the aggregated API with no separate APIService ingress.
func assertManifestSetsEqual(internal, external []unstructured.Unstructured) error {
	intMap, err := manifestChecksumByKey(internal)
	if err != nil {
		return fmt.Errorf("index internal manifests: %w", err)
	}
	extMap, err := manifestChecksumByKey(external)
	if err != nil {
		return fmt.Errorf("index external manifests: %w", err)
	}
	if len(intMap) != len(extMap) {
		return fmt.Errorf("manifest object count differs: internal=%d %v, external=%d %v",
			len(intMap), sortedKeys(intMap), len(extMap), sortedKeys(extMap))
	}
	for _, k := range sortedKeys(intMap) {
		extSum, ok := extMap[k]
		if !ok {
			return fmt.Errorf("external manifests-download is missing object %q present in the internal response", k)
		}
		if extSum != intMap[k] {
			return fmt.Errorf("object %q differs between external and internal manifests-download: internal_sha256=%s external_sha256=%s",
				k, intMap[k], extSum)
		}
	}
	return nil
}
