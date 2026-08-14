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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// --- Coexistence with the storage-volume-data-manager module (E2E_COEXISTENCE) ----------------------
//
// storage-volume-data-manager is a PERMANENT neighbour of storage-foundation, not a migration step:
// operators keep scripts bound to its API group, so both modules stay enabled and each serves its own
// DataExport/DataImport resources — its own API group, its own controller namespace, its own finalizers.
// Two specs pin what has to keep holding while they share a cluster:
//
//  1. `Coexistence 1` — the neighbour's data plane works with storage-foundation enabled, AND bringing
//     storage-foundation through a converge does not touch anything the neighbour owns. A converge is
//     exactly where this used to break: module code that keyed only on the PRESENCE of the neighbour's
//     CRDs treated them as leftovers, and on every converge stripped the neighbour's PVC finalizer,
//     duplicated its in-flight resources under the storage-foundation group and deleted both of its
//     CRDs — taking every user DataExport/DataImport with them, because deleting a CRD cascades. That
//     code is gone; this spec is what keeps it gone.
//
//     The converge MUST be forced explicitly. The suite enables modules once in BeforeSuite and never
//     re-converges them, so a spec that only creates fixtures and re-reads them passes whether or not
//     anything sweeps them: nothing ever gets the chance to sweep. The trigger, and the proof that a
//     converge really happened, are in triggerStorageFoundationConverge.
//
//  2. `Coexistence 2` — a guard on the DOCUMENTED limitation that one target must not be exported
//     through both modules at once: for a LIVE volume both modules derive the same public address, so
//     the second export would publish the address the first one is already serving. The guard asserts
//     that address equality and nothing else — no ingress behaviour, no race outcome.
//
//     It is deliberately SEQUENTIAL (export, read the address, delete, wait for the volume to come back,
//     export through the other module). Two simultaneous exports cannot produce two equal addresses: a
//     live-volume export takes over the volume's PV and publishes its address only AFTER it holds it, so
//     the loser of that contest never publishes at all. And the equality does not exist on snapshot
//     targets — there the two modules already use different path segments — so the guard has to run on a
//     live PVC, whose PV is exactly what makes the protocol sequential.
//
//     If someone later gives the two modules different address schemes this guard fails, and the
//     documented limitation has to be corrected together with it.

const (
	// volumeDataManagerGroup is the API group storage-volume-data-manager serves its DataExport/DataImport
	// resources under. storage-foundation serves ITS resources under its own group (dataExportGVR /
	// dataImportGVR), so the two modules never share an object: same kinds, different groups, different
	// controllers, different namespaces.
	volumeDataManagerGroup = "storage.deckhouse.io"

	// volumeDataManagerDataExportCRD / volumeDataManagerDataImportCRD are the two CRDs that module installs.
	// They must survive a storage-foundation converge as the SAME objects: deleting either one cascades away
	// every DataExport/DataImport a user created through that module, and re-installing the CRD afterwards
	// brings back none of them.
	volumeDataManagerDataExportCRD = "dataexports.storage.deckhouse.io"
	volumeDataManagerDataImportCRD = "dataimports.storage.deckhouse.io"

	// volumeDataManagerPVCFinalizer is the finalizer storage-volume-data-manager puts on a user PVC for the
	// lifetime of an export. It is ACTIVE protection, not bookkeeping: while the export runs, the PVC's PV
	// is detached and rebound into the exporter's own PVC, so a PVC that loses this finalizer mid-export can
	// be deleted out from under live data.
	volumeDataManagerPVCFinalizer = "storage.deckhouse.io/storage-manager-controller"

	// coexistExportTTL is the idle TTL of every DataExport these specs create: comfortably longer than the
	// specs themselves (the exporter resets its idle timer on each request), so no export expires mid-run.
	coexistExportTTL = "60m"
)

// volumeDataManagerExportGVR is the storage-volume-data-manager DataExport resource. That this is a
// DIFFERENT resource from dataExportGVR — not another view of the same objects — is the whole point of the
// coexistence specs.
var volumeDataManagerExportGVR = schema.GroupVersionResource{
	Group: volumeDataManagerGroup, Version: "v1alpha1", Resource: "dataexports",
}

// Coexistence 1 fixture names.
const (
	coexistSourcePVC = "coexist-pvc"
	// coexistSourceFile is the single known file the source PVC carries. One small file is enough: the
	// download proves the exporter serves the volume's bytes, and the bytes are re-read after the converge
	// to prove they are still the same ones.
	coexistSourceFile = "payload.txt"
	coexistWriterPod  = "coexist-writer"
	coexistCurlPod    = "coexist-curl"
	coexistExport     = "coexist-export"
	coexistSA         = "coexist-client"
	// coexistRole names both the Role and the RoleBinding that grant the download identity.
	coexistRole = "coexist-download"
)

// Coexistence 2 (public-address guard) fixture names.
const (
	guardSourcePVC        = "addr-guard-pvc"
	guardBinderPod        = "addr-guard-binder"
	guardFoundationExport = "addr-guard-foundation"
	guardVolumeDataExport = "addr-guard-volume-data"
)

// --- storage-foundation converge: trigger + proof ---------------------------------------------------

const (
	// sfDataManagerDeployment / sfDataManagerContainer are the storage-foundation DataExport/DataImport
	// controller in d8DataManagerNS. Its POD TEMPLATE is the observable that proves a converge actually
	// re-rendered the module's Helm release, rather than the write being a no-op.
	sfDataManagerDeployment = "data-manager-controller"
	sfDataManagerContainer  = "data-manager-controller"

	// sfLogLevelEnv is the env var storage-foundation renders its logLevel setting into on that container.
	// logLevel is the cheapest rendered setting to flip: a plain enum with no effect beyond log verbosity,
	// and it reaches the pod template, so changing it forces a full converge and leaves a visible trace.
	sfLogLevelEnv = "LOG_LEVEL"
)

// sfLogLevelRendered maps a storage-foundation logLevel value to what the module renders into sfLogLevelEnv.
// The spec has to PREDICT the rendered value: finding exactly the predicted value on the pod template is
// what separates "the module re-rendered its release from our setting" from "something else touched the
// Deployment". If the module ever renders logLevel differently, the trigger fails loudly (it refuses to
// proceed from an unrecognised current value) instead of quietly asserting nothing.
var sfLogLevelRendered = map[string]string{
	"ERROR": "0",
	"WARN":  "1",
	"INFO":  "2",
	"DEBUG": "3",
	"TRACE": "4",
}

// sfAlternateLogLevel returns a logLevel whose rendered value differs from the one currently rendered, plus
// that rendered value. Picking by the CURRENT render (not by what the ModuleConfig says) is what guarantees
// the pod template really changes: an unset logLevel still renders the module's own default, and writing
// that same value back would be a no-op with no converge to observe.
func sfAlternateLogLevel(rendered string) (level, wantRendered string) {
	if sfLogLevelRendered["INFO"] != rendered {
		return "INFO", sfLogLevelRendered["INFO"]
	}
	return "TRACE", sfLogLevelRendered["TRACE"]
}

// storageFoundationSettings reads the module's current ModuleConfig spec.settings, so the trigger can put
// them back byte-for-byte afterwards. NestedMap returns a deep copy, so the result is safe to mutate.
func storageFoundationSettings(ctx context.Context) (map[string]interface{}, error) {
	mc, err := suiteDyn.Resource(moduleConfigGVR).Get(ctx, storageFoundationModuleName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get ModuleConfig %s: %w", storageFoundationModuleName, err)
	}
	settings, found, err := unstructured.NestedMap(mc.Object, "spec", "settings")
	if err != nil {
		return nil, fmt.Errorf("read ModuleConfig %s spec.settings: %w", storageFoundationModuleName, err)
	}
	if !found {
		return map[string]interface{}{}, nil
	}
	return settings, nil
}

// applyStorageFoundationSettings writes the module's ModuleConfig spec.settings through the SAME runtime
// path the suite uses to enable modules, so the ModuleConfig ends up exactly as the enable step would have
// written it (create-or-update of the ModuleConfig plus its env-pinned ModulePullOverride, which is
// re-written identically and therefore a no-op).
//
// The ModuleSpec is passed ALONE and WITHOUT Dependencies on purpose: EnableModulesWithSpecs resolves
// Dependencies only inside the set it is handed and aborts with "dependency module ... not found" on a name
// that is not in that set. The enable step's entry for storage-foundation carries a dependency on
// state-snapshotter, so copying that entry into a single-module call would fail before writing anything.
func applyStorageFoundationSettings(ctx context.Context, settings map[string]interface{}) error {
	spec := storagekube.ModuleSpec{
		Name:               storageFoundationModuleName,
		Version:            1,
		Enabled:            true,
		ModulePullOverride: moduleTagFromEnv(storageFoundationModuleName),
		Settings:           settings,
	}
	if err := storagekube.EnableModulesWithSpecs(ctx, suiteClusterResources.Kubeconfig, suiteClusterResources.SSHClient,
		suiteClusterResources.ClusterDefinition, []storagekube.ModuleSpec{spec}); err != nil {
		return fmt.Errorf("apply ModuleConfig settings for module %s: %w", storageFoundationModuleName, err)
	}
	return nil
}

// sfDataManagerGenerationAndLogLevel reports the current pod-template generation of the storage-foundation
// data-manager Deployment and the log level rendered onto its container. An absent env var is returned as
// an empty value with no error; the caller decides whether that is usable (the trigger refuses it, because
// a proof built on an empty expected value would be vacuous).
func sfDataManagerGenerationAndLogLevel(ctx context.Context) (generation int64, logLevel string, err error) {
	deploy, err := suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, sfDataManagerDeployment, metav1.GetOptions{})
	if err != nil {
		return 0, "", fmt.Errorf("get Deployment %s/%s: %w", d8DataManagerNS, sfDataManagerDeployment, err)
	}
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.Name != sfDataManagerContainer {
			continue
		}
		for _, env := range container.Env {
			if env.Name == sfLogLevelEnv {
				return deploy.Generation, env.Value, nil
			}
		}
		return deploy.Generation, "", nil
	}
	return deploy.Generation, "", fmt.Errorf("Deployment %s/%s has no %q container",
		d8DataManagerNS, sfDataManagerDeployment, sfDataManagerContainer)
}

// waitSFDataManagerRerendered blocks until the storage-foundation data-manager Deployment carries a NEW
// pod-template generation AND the expected rendered log level, then until that generation is fully rolled
// out. Both halves are needed: the generation alone would also move for an unrelated edit, and the value
// alone cannot tell a fresh render from the state we started in.
//
// This is the converge proof. The module re-runs its hooks BEFORE the Helm upgrade that writes this pod
// template, so a pod template carrying our value means everything a converge runs has already run — which
// is why the specs wait for this observation instead of sleeping.
//
// The rollout wait deliberately gets its own full `timeout` rather than what is left of this one: the render
// is the slow, uncertain half, and shrinking the rollout budget to its leftovers would fail a converge that
// demonstrably happened. The caller's context bounds the pair either way.
func waitSFDataManagerRerendered(ctx context.Context, afterGeneration int64, wantLogLevel string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		generation, logLevel, err := sfDataManagerGenerationAndLogLevel(ctx)
		switch {
		case err != nil:
			last = err.Error()
		case generation > afterGeneration && logLevel == wantLogLevel:
			GinkgoWriter.Printf("  storage-foundation re-rendered %s/%s: generation %d -> %d, %s=%q\n",
				d8DataManagerNS, sfDataManagerDeployment, afterGeneration, generation, sfLogLevelEnv, logLevel)
			return waitDeploymentRolledOut(ctx, d8DataManagerNS, sfDataManagerDeployment, timeout)
		default:
			last = fmt.Sprintf("generation=%d (want >%d) %s=%q (want %q)",
				generation, afterGeneration, sfLogLevelEnv, logLevel, wantLogLevel)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for the storage-foundation converge to re-render Deployment %s/%s; last: %s",
				d8DataManagerNS, sfDataManagerDeployment, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// triggerStorageFoundationConverge drives storage-foundation through a full module converge (its hooks plus
// a Helm re-render) and returns only once that converge is PROVEN to have happened: a new pod-template
// generation carrying the log level we asked for, the Deployment rolled out, and the module back to Ready.
//
// It also returns a restore function that puts the original settings back and waits for THAT converge the
// same way. restore is returned even on error, so a caller that fails midway can still hand the cluster
// back in its original shape.
//
// Forcing a converge is not optional. The suite enables modules once, in BeforeSuite, and nothing
// re-converges them afterwards; anything that runs on a module converge therefore never runs again during
// the suite. Without this trigger, "the neighbour module's resources are still here" holds trivially and
// the spec would stay green even against module code that eats them on every converge.
func triggerStorageFoundationConverge(ctx context.Context) (restore func(context.Context) error, err error) {
	originalSettings, err := storageFoundationSettings(ctx)
	if err != nil {
		return nil, err
	}
	beforeGeneration, beforeLogLevel, err := sfDataManagerGenerationAndLogLevel(ctx)
	if err != nil {
		return nil, err
	}
	// An unrecognised (or missing) rendered value means the module no longer renders logLevel the way this
	// trigger assumes. Stop here: continuing would compare the pod template against a value the module never
	// writes, which either hangs on the deadline or, worse, passes without a converge.
	if levelForRendered(beforeLogLevel) == "" {
		return nil, fmt.Errorf(
			"Deployment %s/%s renders %s=%q, which is not one of the storage-foundation logLevel values this spec knows (%v); the converge trigger needs a predictable rendered value",
			d8DataManagerNS, sfDataManagerDeployment, sfLogLevelEnv, beforeLogLevel, sfLogLevelRendered)
	}

	nextLevel, wantRendered := sfAlternateLogLevel(beforeLogLevel)
	nextSettings := map[string]interface{}{}
	for key, value := range originalSettings {
		nextSettings[key] = value
	}
	nextSettings["logLevel"] = nextLevel

	restore = func(rctx context.Context) error {
		generation, currentLogLevel, gerr := sfDataManagerGenerationAndLogLevel(rctx)
		if gerr != nil {
			return gerr
		}
		if aerr := applyStorageFoundationSettings(rctx, originalSettings); aerr != nil {
			return aerr
		}
		if currentLogLevel == beforeLogLevel {
			// The forward trigger never landed (it failed before the module re-rendered), so writing the
			// original settings back is a no-op and NO new generation will ever appear. Waiting for one here
			// would burn the whole timeout and then report a failure that is not the one worth reporting.
			return storagekube.WaitForModuleReady(rctx, suiteRestCfg, storageFoundationModuleName, suiteCfg.moduleReadyTO)
		}
		if werr := waitSFDataManagerRerendered(rctx, generation, beforeLogLevel, suiteCfg.moduleReadyTO); werr != nil {
			return fmt.Errorf("storage-foundation did not converge back to its original logLevel: %w", werr)
		}
		return storagekube.WaitForModuleReady(rctx, suiteRestCfg, storageFoundationModuleName, suiteCfg.moduleReadyTO)
	}

	GinkgoWriter.Printf("  forcing a storage-foundation converge: logLevel -> %s (expecting %s=%q on %s/%s, generation >%d)\n",
		nextLevel, sfLogLevelEnv, wantRendered, d8DataManagerNS, sfDataManagerDeployment, beforeGeneration)
	if aerr := applyStorageFoundationSettings(ctx, nextSettings); aerr != nil {
		return restore, aerr
	}
	if werr := waitSFDataManagerRerendered(ctx, beforeGeneration, wantRendered, suiteCfg.moduleReadyTO); werr != nil {
		return restore, werr
	}
	if merr := storagekube.WaitForModuleReady(ctx, suiteRestCfg, storageFoundationModuleName, suiteCfg.moduleReadyTO); merr != nil {
		return restore, fmt.Errorf("storage-foundation did not return to Ready after the converge: %w", merr)
	}
	return restore, nil
}

// levelForRendered reverses sfLogLevelRendered, so an unexpected rendered value can be rejected before it is
// used as a baseline. Returns "" when nothing renders to value, which is itself not a known level.
func levelForRendered(value string) string {
	for level, rendered := range sfLogLevelRendered {
		if rendered == value {
			return level
		}
	}
	return ""
}

// --- storage-volume-data-manager DataExport helpers ------------------------------------------------

// createVolumeDataManagerExport creates a DataExport on the storage-volume-data-manager API group against a live
// PVC. The target reference is that module's own shape — {kind, name} with NO group field — which is one of
// the things that makes these objects unmistakably its own and not storage-foundation's.
func createVolumeDataManagerExport(ctx context.Context, ns, name, pvcName string, publish bool) error {
	export := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": volumeDataManagerExportGVR.GroupVersion().String(),
		"kind":       "DataExport",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"ttl":     coexistExportTTL,
			"publish": publish,
			"targetRef": map[string]interface{}{
				"kind": "PersistentVolumeClaim",
				"name": pvcName,
			},
		},
	}}
	if _, err := suiteDyn.Resource(volumeDataManagerExportGVR).Namespace(ns).Create(ctx, export, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create DataExport %s/%s on group %s: %w", ns, name, volumeDataManagerGroup, err)
	}
	return nil
}

// waitVolumeDataManagerExportReady polls a storage-volume-data-manager DataExport until it is Ready with
// status.url (and, when wantPublicURL is set, status.publicURL) populated, and returns the URLs plus the
// PEM-normalized status.ca. It is deliberately NOT waitDataExportReady: that one also requires
// status.phase, which is part of the storage-foundation status model and not of this module's.
func waitVolumeDataManagerExportReady(ctx context.Context, ns, name string, wantPublicURL bool, timeout time.Duration) (url, publicURL, ca string, err error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, gerr := getResource(ctx, volumeDataManagerExportGVR, ns, name)
		if gerr == nil {
			status, reason, found := conditionStatus(obj, "Ready")
			if found && status == "True" {
				url, _, _ = unstructured.NestedString(obj.Object, "status", "url")
				publicURL, _, _ = unstructured.NestedString(obj.Object, "status", "publicURL")
				rawCA, _, _ := unstructured.NestedString(obj.Object, "status", "ca")
				ca = normalizePublishCA(rawCA)
				if url != "" && ca != "" && (!wantPublicURL || publicURL != "") {
					return url, publicURL, ca, nil
				}
				last = fmt.Sprintf("Ready=True but url=%q publicURL=%q ca=%t", url, publicURL, ca != "")
			} else {
				last = fmt.Sprintf("Ready=%q reason=%q", status, reason)
			}
		} else {
			last = fmt.Sprintf("get err=%v", gerr)
		}
		if time.Now().After(deadline) {
			return "", "", "", fmt.Errorf("timeout waiting for DataExport %s/%s (group %s) to become Ready; last: %s",
				ns, name, volumeDataManagerGroup, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", "", "", ctx.Err()
		}
	}
}

func deleteVolumeDataManagerExport(ctx context.Context, ns, name string) {
	_ = suiteDyn.Resource(volumeDataManagerExportGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

// waitDataExportGone blocks until a DataExport object is really gone, which is the exact signal that the
// export released the volume: both modules rebind the PV to the user PVC, drop the PVC finalizer and reap
// the publish resources BEFORE they remove their own finalizer, so the object disappearing means that
// whole release finished. Polling the PVC alone would race the tail of it.
func waitDataExportGone(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		_, err := getResource(ctx, gvr, ns, name)
		switch {
		case err == nil:
			last = "still present"
		case errIsNotFound(err):
			return nil
		default:
			last = fmt.Sprintf("get err=%v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s %s/%s to be deleted; last: %s", gvr.Resource, ns, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// createVolumeDataManagerDownloadRole grants `create` on dataexports/download in the storage-volume-data-manager API
// group. The exporter authorizes every request with a SubjectAccessReview against the DataExport's OWN API
// group, so the group here MUST be that module's; a Role on the storage-foundation group (what
// createDataDownloadRole grants) leaves the request Forbidden.
func createVolumeDataManagerDownloadRole(ctx context.Context, ns string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: coexistRole, Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{volumeDataManagerGroup},
			Resources: []string{volumeDataManagerExportGVR.Resource + "/download"},
			Verbs:     []string{"create"},
		}},
	}
	if _, err := suiteClientset.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create download Role %s/%s: %w", ns, coexistRole, err)
	}
	return nil
}

// --- fixture helpers --------------------------------------------------------------------------------

// writeCoexistSourceFile writes the single known file into the source PVC mounted by the writer pod and
// returns its sha256, read back with sha256sum in that same pod. The content is passed as an exec argv value
// (never interpolated into the shell) and written with `printf '%s'` (no trailing newline), so the digest
// covers exactly those bytes.
func writeCoexistSourceFile(ctx context.Context, ns, content string) (string, error) {
	// $0=sh placeholder, $1=mount dir, $2=file content.
	script := `set -e; cd "$1"; printf '%s' "$2" > ` + coexistSourceFile + `; sync; sha256sum ` + coexistSourceFile
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, ns, coexistWriterPod, vdProbeContainer, []string{
		"sh", "-c", script, "sh", "/mnt/" + coexistSourcePVC, content,
	})
	if err != nil {
		return "", fmt.Errorf("write %s in %s/%s: %w (stderr=%q)", coexistSourceFile, ns, coexistWriterPod, err, stderr)
	}
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("unexpected sha256sum output %q for %s in %s/%s", stdout, coexistSourceFile, ns, coexistWriterPod)
	}
	return fields[0], nil
}

// coexistSourcePVCFinalizers returns the finalizers currently on the Coexistence 1 source PVC.
func coexistSourcePVCFinalizers(ctx context.Context, ns string) ([]string, error) {
	pvc, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, coexistSourcePVC, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get PVC %s/%s: %w", ns, coexistSourcePVC, err)
	}
	return pvc.Finalizers, nil
}

// assertCoexistExportServesSource downloads the source file through the exporter and asserts its sha256
// equals the source digest, streaming the body into sha256sum inside the runner pod.
//
// A fresh Bearer token is minted on every attempt, and the whole request is retried: right after the
// RoleBinding the exporter's SubjectAccessReview is still eventually consistent, and the second call site
// runs after a module converge, which can outlive a token minted before it.
func assertCoexistExportServesSource(ctx context.Context, target curlPodTarget, ns, fileURL, caFile, wantSum string) {
	GinkgoHelper()
	Eventually(func() (string, error) {
		token, terr := issueServiceAccountToken(ctx, ns, coexistSA)
		if terr != nil {
			return "", terr
		}
		return runCurlChecksum(ctx, target, fileURL, curlRequest{token: token, caFile: caFile})
	}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(pollInterval).Should(Equal(wantSum),
		"the file downloaded from the storage-volume-data-manager exporter must match the source sha256")
}

// waitGuardPVCRebound blocks until the guard's source PVC is Bound to its ORIGINAL PV again and that PV's
// claimRef points back at the PVC. This is the precondition the next live-PVC export validates before it
// takes the volume over, so the sequential protocol waits for it rather than assuming the previous export's
// teardown already settled.
func waitGuardPVCRebound(ctx context.Context, ns, pvName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		pvc, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, guardSourcePVC, metav1.GetOptions{})
		switch {
		case err != nil:
			last = fmt.Sprintf("get PVC err=%v", err)
		case pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName != pvName:
			last = fmt.Sprintf("PVC phase=%s volumeName=%q (want Bound to %q)", pvc.Status.Phase, pvc.Spec.VolumeName, pvName)
		default:
			pv, pverr := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
			switch {
			case pverr != nil:
				last = fmt.Sprintf("get PV err=%v", pverr)
			case pv.Spec.ClaimRef == nil:
				last = "PV has no claimRef"
			case pv.Spec.ClaimRef.Namespace == ns && pv.Spec.ClaimRef.Name == guardSourcePVC:
				return nil
			default:
				last = fmt.Sprintf("PV claimRef=%s/%s (want %s/%s)",
					pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name, ns, guardSourcePVC)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for PVC %s/%s to be bound to PV %s again; last: %s", ns, guardSourcePVC, pvName, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// --- Coexistence 1: the neighbour module's data plane survives a storage-foundation converge --------

// coexistenceSpecs registers the storage-volume-data-manager coexistence flow (opt-out E2E_COEXISTENCE).
// Ordered: the source volume is built once, spec (a) puts a live export on it through the neighbour module,
// and spec (b) drives storage-foundation through a converge underneath that live export.
func coexistenceSpecs() {
	Context("Coexistence 1: storage-volume-data-manager serves its own resources next to storage-foundation", Ordered, func() {
		var (
			srcNS     string
			srcSum    string
			fileURL   string
			caFile    string
			curlIn    curlPodTarget
			exportUID types.UID
		)

		BeforeAll(func() {
			if !suiteCfg.coexistence {
				Skip(envCoexistence + "=false: skipping the storage-volume-data-manager coexistence specs (they run by default)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			By("Ensuring a thin, snapshot-capable StorageClass (" + suiteCfg.storageClass + ")")
			// Idempotent: other phases provision the same class under their own knobs, so provision it here
			// rather than depend on one of them having run.
			Expect(ensureSnapshotStorageClass(ctx, suiteCfg.storageClass)).To(Succeed())

			srcNS = uniqueNS("coexist")
			By("Creating the source namespace " + srcNS)
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Creating the source Filesystem PVC " + coexistSourcePVC + " and writing one known file")
			Expect(createFilesystemPVC(ctx, srcNS, coexistSourcePVC, suiteCfg.storageClass, "1Gi")).To(Succeed())
			// The writer pod is the first consumer that binds the WaitForFirstConsumer PVC; waitPodRunning
			// blocks until that bind completes.
			_, err := suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, coexistWriterPod, []string{coexistSourcePVC}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create the source writer pod")
			Expect(waitPodRunning(ctx, srcNS, coexistWriterPod, 10*time.Minute)).To(Succeed())
			sum, werr := writeCoexistSourceFile(ctx, srcNS, "coexistence payload "+srcNS)
			Expect(werr).NotTo(HaveOccurred(), "write the source file and record its checksum")
			srcSum = sum

			By("Deleting the source writer pod (a live-PVC export takes over the PV, so no consumer may hold it)")
			forceDeletePod(ctx, srcNS, coexistWriterPod)
			Expect(waitPodDeleted(ctx, srcNS, coexistWriterPod, 3*time.Minute)).To(Succeed())

			// The DataExport is created in (a) and still live through (b), so its teardown is container-scoped,
			// not spec-scoped. It is registered AFTER the namespace cleanup so it runs BEFORE it (Ginkgo runs
			// DeferCleanup nodes in reverse registration order): an export still holding the PVC's PV — and the
			// PVC's finalizer — must release both before the namespace is deleted, otherwise the namespace sits
			// in Terminating. deleteVolumeDataManagerExport is idempotent and the keep-cluster knobs are honored.
			DeferCleanup(func() {
				if cleanupSkipped() {
					GinkgoWriter.Printf("%s: keeping DataExport %s/%s\n", keepReason(), srcNS, coexistExport)
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer ccancel()
				deleteVolumeDataManagerExport(cctx, srcNS, coexistExport)
				if err := waitDataExportGone(cctx, volumeDataManagerExportGVR, srcNS, coexistExport, 10*time.Minute); err != nil {
					GinkgoWriter.Printf("  warning: DataExport %s/%s did not finish releasing the volume: %v\n", srcNS, coexistExport, err)
				}
			})
		})

		It("(a) exports a live PVC through storage-volume-data-manager while storage-foundation is enabled", func() {
			ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.dataTransferTO+20*time.Minute)
			defer cancel()

			By("Creating the DataExport on the storage-volume-data-manager group (targetRef {kind,name}, no group field)")
			Expect(createVolumeDataManagerExport(ctx, srcNS, coexistExport, coexistSourcePVC, false)).To(Succeed())

			By("Waiting for it to reach Ready with status.url + status.ca")
			intURL, _, ca, werr := waitVolumeDataManagerExportReady(ctx, srcNS, coexistExport, false, suiteCfg.dataTransferTO)
			Expect(werr).NotTo(HaveOccurred(), "DataExport %s/%s Ready", srcNS, coexistExport)
			GinkgoWriter.Printf("  DataExport %s/%s (group %s) status.url=%s\n", srcNS, coexistExport, volumeDataManagerGroup, intURL)

			export, gerr := getResource(ctx, volumeDataManagerExportGVR, srcNS, coexistExport)
			Expect(gerr).NotTo(HaveOccurred(), "read the published DataExport back")
			exportUID = export.GetUID()
			Expect(exportUID).NotTo(BeEmpty(), "the DataExport must carry a uid to compare across the converge")

			By("Asserting the export holds the volume: the source PVC carries that module's own finalizer")
			Eventually(func() ([]string, error) {
				return coexistSourcePVCFinalizers(ctx, srcNS)
			}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(pollInterval).Should(ContainElement(volumeDataManagerPVCFinalizer),
				"while the PV is detached into the exporter, the module protects the source PVC with its own finalizer")

			By("Creating the in-cluster curl pod and the download identity")
			_, cerr := suiteClientset.CoreV1().Pods(srcNS).Create(ctx, nestedCurlPodSpec(srcNS, coexistCurlPod), metav1.CreateOptions{})
			Expect(cerr).NotTo(HaveOccurred(), "create the in-cluster curl pod")
			Expect(waitPodRunning(ctx, srcNS, coexistCurlPod, 5*time.Minute)).To(Succeed())
			curlIn = nestedCurlTarget(srcNS, coexistCurlPod, publishDEIntCont)

			// The exporter's serving certificate is issued by its own CA, published as status.ca, so the
			// download validates it with --cacert instead of skipping verification.
			materializedCA, caErr := writeCAToPodFile(ctx, curlIn, ca)
			Expect(caErr).NotTo(HaveOccurred(), "materialize status.ca inside the curl pod")
			caFile = materializedCA

			Expect(createServiceAccountIfNotExists(ctx, srcNS, coexistSA)).To(Succeed())
			Expect(createVolumeDataManagerDownloadRole(ctx, srcNS)).To(Succeed())
			Expect(bindRoleToServiceAccount(ctx, srcNS, coexistRole, coexistRole, srcNS, coexistSA)).To(Succeed())

			By("Downloading the file through the exporter and matching the source checksum")
			fileURL = publishDataURL(intURL, publishFilesPath+coexistSourceFile)
			assertCoexistExportServesSource(ctx, curlIn, srcNS, fileURL, caFile, srcSum)
		})

		It("(b) keeps everything storage-volume-data-manager owns intact across a storage-foundation converge", func() {
			Expect(exportUID).NotTo(BeEmpty(), "spec (a) must have published the DataExport this spec converges storage-foundation underneath")
			Expect(fileURL).NotTo(BeEmpty(), "spec (a) must have resolved the exporter download URL")

			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.moduleReadyTO+30*time.Minute)
			defer cancel()

			volumeDataManagerCRDs := []string{volumeDataManagerDataExportCRD, volumeDataManagerDataImportCRD}

			By("Recording the identity of both CRDs the module installs")
			crdUIDs := map[string]types.UID{}
			for _, crd := range volumeDataManagerCRDs {
				obj, err := getResource(ctx, crdGVR, "", crd)
				Expect(err).NotTo(HaveOccurred(), "CRD %s must exist while storage-volume-data-manager is enabled", crd)
				Expect(obj.GetUID()).NotTo(BeEmpty(), "CRD %s must carry a uid", crd)
				crdUIDs[crd] = obj.GetUID()
			}

			By("Forcing storage-foundation through a converge and waiting for proof that it happened")
			restore, terr := triggerStorageFoundationConverge(ctx)
			if restore != nil {
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), suiteCfg.moduleReadyTO+10*time.Minute)
					defer ccancel()
					Expect(restore(cctx)).To(Succeed(), "the storage-foundation logLevel must be put back and its converge observed")
				})
			}
			Expect(terr).NotTo(HaveOccurred(), "force a storage-foundation converge")

			By("Asserting both CRDs survived as the SAME objects")
			for _, crd := range volumeDataManagerCRDs {
				obj, err := getResource(ctx, crdGVR, "", crd)
				Expect(err).NotTo(HaveOccurred(), "CRD %s must still exist after a storage-foundation converge", crd)
				Expect(obj.GetUID()).To(Equal(crdUIDs[crd]),
					"CRD %s must not be deleted and reinstalled: the delete cascades away every DataExport/DataImport in the cluster, and reinstalling brings none of them back", crd)
			}

			By("Asserting the live DataExport is the same object and still Ready")
			export, gerr := getResource(ctx, volumeDataManagerExportGVR, srcNS, coexistExport)
			Expect(gerr).NotTo(HaveOccurred(), "DataExport %s/%s must survive a storage-foundation converge", srcNS, coexistExport)
			Expect(export.GetUID()).To(Equal(exportUID),
				"DataExport %s/%s must be the same object, not a replacement created under another owner", srcNS, coexistExport)
			status, reason, found := conditionStatus(export, "Ready")
			Expect(found).To(BeTrue(), "DataExport %s/%s must still report a Ready condition", srcNS, coexistExport)
			Expect(status).To(Equal("True"), "DataExport %s/%s must still be Ready (reason=%q)", srcNS, coexistExport, reason)

			By("Asserting the source PVC still carries the module's finalizer")
			finalizers, ferr := coexistSourcePVCFinalizers(ctx, srcNS)
			Expect(ferr).NotTo(HaveOccurred())
			Expect(finalizers).To(ContainElement(volumeDataManagerPVCFinalizer),
				"the volume protection of the exporting module must not be swept by another module's converge")

			By("Asserting the export still serves the same bytes")
			assertCoexistExportServesSource(ctx, curlIn, srcNS, fileURL, caFile, srcSum)
		})
	})
}

// --- Coexistence 2: the public-address guard --------------------------------------------------------

// publicAddressSchemeGuardSpecs registers the guard on the documented limitation that one live target must
// not be exported through both modules at once (opt-out E2E_COEXISTENCE; also needs the publish
// infrastructure, since the address it compares is status.publicURL).
func publicAddressSchemeGuardSpecs() {
	Context("Coexistence 2: both modules publish one live PVC under the same public address", Ordered, func() {
		var (
			srcNS string
			srcPV string
		)

		BeforeAll(func() {
			if !suiteCfg.coexistence {
				Skip(envCoexistence + "=false: skipping the public-address guard (it runs by default)")
			}
			if !suiteCfg.publish {
				Skip(envPublish + "=false: the guard compares status.publicURL, which exists only with the publish infrastructure")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			By("Ensuring a thin, snapshot-capable StorageClass (" + suiteCfg.storageClass + ")")
			Expect(ensureSnapshotStorageClass(ctx, suiteCfg.storageClass)).To(Succeed())

			srcNS = uniqueNS("addr-guard")
			By("Creating the source namespace " + srcNS)
			Expect(ensureNamespace(ctx, srcNS)).To(Succeed())
			DeferCleanup(func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer ccancel()
				deleteNamespace(cctx, srcNS)
			})

			By("Creating and binding the source Filesystem PVC " + guardSourcePVC)
			// The guard reads addresses only, so the volume needs no content — but it must be BOUND, because
			// a live-PVC export publishes only after it has taken the PV over.
			Expect(createFilesystemPVC(ctx, srcNS, guardSourcePVC, suiteCfg.storageClass, "1Gi")).To(Succeed())
			_, err := suiteClientset.CoreV1().Pods(srcNS).Create(ctx, probePodSpec(srcNS, guardBinderPod, []string{guardSourcePVC}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "create the PVC binder pod")
			Expect(waitPodRunning(ctx, srcNS, guardBinderPod, 10*time.Minute)).To(Succeed())

			pvc, gerr := suiteClientset.CoreV1().PersistentVolumeClaims(srcNS).Get(ctx, guardSourcePVC, metav1.GetOptions{})
			Expect(gerr).NotTo(HaveOccurred(), "read the bound source PVC")
			srcPV = pvc.Spec.VolumeName
			Expect(srcPV).NotTo(BeEmpty(), "the source PVC must be bound to a PV before it can be exported")

			By("Deleting the binder pod (a live-PVC export takes over the PV, so no consumer may hold it)")
			forceDeletePod(ctx, srcNS, guardBinderPod)
			Expect(waitPodDeleted(ctx, srcNS, guardBinderPod, 3*time.Minute)).To(Succeed())

			// The spec deletes both exports itself; these are idempotent safety nets for a spec that failed
			// midway. Registered AFTER the namespace cleanup so they run BEFORE it (reverse registration
			// order): an export still holding the PV must release it before the namespace is deleted.
			DeferCleanup(func() {
				if cleanupSkipped() {
					GinkgoWriter.Printf("%s: keeping the DataExports in %s\n", keepReason(), srcNS)
					return
				}
				cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer ccancel()
				deleteDataExport(cctx, srcNS, guardFoundationExport)
				deleteVolumeDataManagerExport(cctx, srcNS, guardVolumeDataExport)
				if err := waitDataExportGone(cctx, dataExportGVR, srcNS, guardFoundationExport, 5*time.Minute); err != nil {
					GinkgoWriter.Printf("  warning: DataExport %s/%s did not finish releasing the volume: %v\n", srcNS, guardFoundationExport, err)
				}
				if err := waitDataExportGone(cctx, volumeDataManagerExportGVR, srcNS, guardVolumeDataExport, 5*time.Minute); err != nil {
					GinkgoWriter.Printf("  warning: DataExport %s/%s did not finish releasing the volume: %v\n", srcNS, guardVolumeDataExport, err)
				}
			})
		})

		It("publishes the SAME public address from both modules for one live PVC (one export at a time)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*suiteCfg.dataTransferTO+40*time.Minute)
			defer cancel()

			// Both addresses are compared against this independently derived expectation, not just against
			// each other: two empty strings are equal too, and that must not pass as a match.
			wantURL := fmt.Sprintf("https://%s/%s/pvc/%s/", suitePublishInfra.originIngressHost, srcNS, guardSourcePVC)

			By("Publishing the PVC through storage-foundation and reading status.publicURL")
			Expect(createPublishDataExportPVC(ctx, srcNS, guardFoundationExport, guardSourcePVC, coexistExportTTL)).To(Succeed())
			_, sfURL, _, sferr := waitDataExportPublished(ctx, srcNS, guardFoundationExport, suiteCfg.dataTransferTO)
			Expect(sferr).NotTo(HaveOccurred(), "storage-foundation DataExport %s/%s Ready + published", srcNS, guardFoundationExport)
			Expect(sfURL).To(Equal(wantURL), "storage-foundation status.publicURL")

			By("Deleting that export and waiting for the PV to return to the source PVC")
			deleteDataExport(ctx, srcNS, guardFoundationExport)
			Expect(waitDataExportGone(ctx, dataExportGVR, srcNS, guardFoundationExport, 10*time.Minute)).To(Succeed())
			Expect(waitGuardPVCRebound(ctx, srcNS, srcPV, 10*time.Minute)).To(Succeed())

			By("Publishing the SAME PVC through storage-volume-data-manager and reading its status.publicURL")
			Expect(createVolumeDataManagerExport(ctx, srcNS, guardVolumeDataExport, guardSourcePVC, true)).To(Succeed())
			_, volumeDataURL, _, vdmErr := waitVolumeDataManagerExportReady(ctx, srcNS, guardVolumeDataExport, true, suiteCfg.dataTransferTO)
			Expect(vdmErr).NotTo(HaveOccurred(), "storage-volume-data-manager DataExport %s/%s Ready + published", srcNS, guardVolumeDataExport)

			By("Asserting both modules derived the same public address for the same volume")
			Expect(volumeDataURL).To(Equal(wantURL), "storage-volume-data-manager status.publicURL")
			Expect(volumeDataURL).To(Equal(sfURL),
				"both modules publish a live volume under the same host and path, which is why one target must not be exported through both at once; if the schemes are ever split apart, the documented limitation has to be corrected with this guard")

			By("Deleting the second export and waiting for the PV to return to the source PVC")
			deleteVolumeDataManagerExport(ctx, srcNS, guardVolumeDataExport)
			Expect(waitDataExportGone(ctx, volumeDataManagerExportGVR, srcNS, guardVolumeDataExport, 10*time.Minute)).To(Succeed())
			Expect(waitGuardPVCRebound(ctx, srcNS, srcPV, 10*time.Minute)).To(Succeed())
		})
	})
}
