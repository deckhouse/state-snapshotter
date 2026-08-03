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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Failure diagnostics for the recovery scenarios.
//
// The suite-wide dump (dumpFailedSpecDiagnostics) is built for the snapshot tree and says nothing about a
// borrowed volume. These scenarios fail on the state of five objects instead — the export, the two claims,
// the volume and whatever holds it — and a failure is only worth acting on once that state has been read.
// So the report below prints the facts first, in one place, before anyone reaches for the state machine:
// what the spec expected is in its own text, and this is what the cluster actually held.
//
// It is deliberately read-only and best-effort: a diagnostic that fails, or that mutates anything, is worse
// than none.

// deRecDiagnostics registers the report for one scenario. ns is read through a closure because it is
// generated per run and assigned in BeforeAll; the export name is a constant of the scenario.
//
// A Context-level AfterEach runs before the suite-wide one (Ginkgo unwinds innermost first), so this
// report lands above the generic dump rather than under it.
func deRecDiagnostics(ns func() string, exportName string) {
	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		dumpDataExportRecovery(ctx, ns(), exportName)
	})
}

// deRecFixtureDiagnostics is deRecDiagnostics for the scenarios that keep their state in a fixture: the
// pointer is nil until BeforeAll has built it, and a failure before that still has to produce a report
// (saying which stage was not reached) instead of a nil dereference inside the hook.
func deRecFixtureDiagnostics(fixture **deRecExportFixture, exportName string) {
	deRecDiagnostics(func() string {
		if *fixture == nil {
			return ""
		}
		return (*fixture).ns
	}, exportName)
}

// dumpDataExportRecovery prints the compact per-scenario report: the export's own verdict, the identity it
// recorded, the live state of both claims and the volume, who is holding what, and the controller lines
// about exactly these objects.
func dumpDataExportRecovery(ctx context.Context, ns, exportName string) {
	GinkgoWriter.Printf("\n========== DataExport recovery report ==========\n")
	GinkgoWriter.Printf("scenario: %s\n", CurrentSpecReport().FullText())
	if ns == "" {
		GinkgoWriter.Printf("namespace unknown (the fixture failed before it was created); nothing to report\n")
		GinkgoWriter.Printf("===============================================\n\n")
		return
	}

	facts := readRecoveryFacts(ctx, ns, exportName)
	GinkgoWriter.Printf("%s", facts.String())

	// Only lines about these objects: the controller serves every export in the cluster, and an unfiltered
	// tail from a parallel scenario is what makes a report unreadable.
	filters := []string{exportName, facts.exportPVCName, facts.deployName}
	if facts.pvName != "" {
		filters = append(filters, facts.pvName)
	}
	dumpFilteredControllerLogs(ctx, d8DataManagerNS, filters...)
	GinkgoWriter.Printf("===============================================\n\n")
}

// recoveryFacts is the state of one export, read once so the report cannot contradict itself.
type recoveryFacts struct {
	ns, exportName string
	exportPVCName  string
	deployName     string
	pvName         string

	deFound         bool
	deErr           error
	phase           string
	readyStatus     string
	readyReason     string
	readyMessage    string
	cleanupReason   string
	deleting        bool
	deFinalizers    []string
	recovery        map[string]string
	recoveryPresent bool

	sourceClaim *corev1.PersistentVolumeClaim
	exportClaim *corev1.PersistentVolumeClaim
	pv          *corev1.PersistentVolume

	claimHolders []corev1.Pod
	deployFound  bool
	deployReady  int32
	attachments  []storageAttachment
}

// storageAttachment is the part of a VolumeAttachment that decides whether B2 is passable.
type storageAttachment struct {
	name       string
	attached   bool
	deleting   bool
	finalizers []string
}

func readRecoveryFacts(ctx context.Context, ns, exportName string) *recoveryFacts {
	f := &recoveryFacts{
		ns:            ns,
		exportName:    exportName,
		exportPVCName: generatedExportName("pvc-for", ns, exportName),
		deployName:    generatedExportName("deploy-for", ns, exportName),
		recovery:      map[string]string{},
	}

	if obj, err := getResource(ctx, dataExportGVR, ns, exportName); err == nil {
		f.deFound = true
		f.phase, _, _ = unstructured.NestedString(obj.Object, "status", "phase")
		f.cleanupReason, _, _ = unstructured.NestedString(obj.Object, "status", "cleanupReason")
		f.readyStatus, f.readyReason, _ = conditionStatus(obj, "Ready")
		f.readyMessage = conditionMessage(obj, "Ready")
		f.deleting = obj.GetDeletionTimestamp() != nil
		f.deFinalizers = obj.GetFinalizers()
		if rec, found, _ := unstructured.NestedMap(obj.Object, "status", "recovery"); found {
			f.recoveryPresent = true
			for _, key := range []string{"sourcePVCUID", "exportPVCUID", "pvName", "pvUID"} {
				f.recovery[key], _, _ = unstructured.NestedString(rec, key)
			}
			f.pvName = f.recovery["pvName"]
		}
	} else {
		f.deErr = err
	}

	if claim, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, deRecPVC, metav1.GetOptions{}); err == nil {
		f.sourceClaim = claim
		// Before the takeover (and after the volume is returned) the claim is the only place the volume's
		// name can be read; status.recovery has it only in between.
		if f.pvName == "" {
			f.pvName = claim.Spec.VolumeName
		}
	}
	if claim, err := suiteClientset.CoreV1().PersistentVolumeClaims(d8DataManagerNS).Get(ctx, f.exportPVCName, metav1.GetOptions{}); err == nil {
		f.exportClaim = claim
		if f.pvName == "" {
			f.pvName = claim.Spec.VolumeName
		}
	}
	if f.pvName != "" {
		if pv, err := suiteClientset.CoreV1().PersistentVolumes().Get(ctx, f.pvName, metav1.GetOptions{}); err == nil {
			f.pv = pv
		}
	}

	// Every pod referencing the export claim, by the same rule B1 uses — and read live, across the whole
	// namespace, because a pod that is not the module's own is exactly the kind that blocks a recovery.
	if pods, err := suiteClientset.CoreV1().Pods(d8DataManagerNS).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range pods.Items {
			for _, volume := range pods.Items[i].Spec.Volumes {
				if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == f.exportPVCName {
					f.claimHolders = append(f.claimHolders, pods.Items[i])
					break
				}
			}
		}
	}
	if deploy, err := suiteClientset.AppsV1().Deployments(d8DataManagerNS).Get(ctx, f.deployName, metav1.GetOptions{}); err == nil {
		f.deployFound = true
		f.deployReady = deploy.Status.AvailableReplicas
	}
	if f.pvName != "" {
		if list, err := suiteClientset.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{}); err == nil {
			for i := range list.Items {
				source := list.Items[i].Spec.Source.PersistentVolumeName
				if source == nil || *source != f.pvName {
					continue
				}
				f.attachments = append(f.attachments, storageAttachment{
					name:       list.Items[i].Name,
					attached:   list.Items[i].Status.Attached,
					deleting:   list.Items[i].DeletionTimestamp != nil,
					finalizers: list.Items[i].Finalizers,
				})
			}
		}
	}
	return f
}

func (f *recoveryFacts) String() string {
	var b strings.Builder

	if !f.deFound {
		fmt.Fprintf(&b, "DataExport %s/%s: <not readable: %v>\n", f.ns, f.exportName, f.deErr)
	} else {
		fmt.Fprintf(&b, "DataExport %s/%s: phase=%q Ready=%s reason=%q\n", f.ns, f.exportName, f.phase, f.readyStatus, f.readyReason)
		fmt.Fprintf(&b, "    message: %s\n", f.readyMessage)
		fmt.Fprintf(&b, "    cleanupReason=%q deleting=%v finalizers=%v\n", f.cleanupReason, f.deleting, f.deFinalizers)
		if f.recoveryPresent {
			fmt.Fprintf(&b, "    recovery: pvName=%s pvUID=%s exportPVCUID=%s sourcePVCUID=%s\n",
				f.recovery["pvName"], f.recovery["pvUID"], f.recovery["exportPVCUID"], f.recovery["sourcePVCUID"])
		} else {
			fmt.Fprintf(&b, "    recovery: <absent> (no takeover identity recorded)\n")
		}
	}

	fmt.Fprintf(&b, "source claim %s/%s: %s\n", f.ns, deRecPVC, describeClaim(f.sourceClaim))
	fmt.Fprintf(&b, "export claim %s/%s: %s\n", d8DataManagerNS, f.exportPVCName, describeClaim(f.exportClaim))
	fmt.Fprintf(&b, "volume %s: %s\n", orNone(f.pvName), describeVolume(f.pv))

	if !f.deployFound {
		fmt.Fprintf(&b, "exporter Deployment %s: <absent>\n", f.deployName)
	} else {
		fmt.Fprintf(&b, "exporter Deployment %s: availableReplicas=%d\n", f.deployName, f.deployReady)
	}

	if len(f.claimHolders) == 0 {
		fmt.Fprintf(&b, "pods holding the export claim (B1): <none>\n")
	}
	for i := range f.claimHolders {
		pod := &f.claimHolders[i]
		fmt.Fprintf(&b, "pods holding the export claim (B1): %s phase=%s deleting=%v finalizers=%v\n",
			pod.Name, pod.Status.Phase, pod.DeletionTimestamp != nil, pod.Finalizers)
	}

	if len(f.attachments) == 0 {
		fmt.Fprintf(&b, "VolumeAttachment for the volume (B2): <none>\n")
	}
	for _, attachment := range f.attachments {
		fmt.Fprintf(&b, "VolumeAttachment for the volume (B2): %s attached=%v deleting=%v finalizers=%v\n",
			attachment.name, attachment.attached, attachment.deleting, attachment.finalizers)
	}

	fmt.Fprintf(&b, "checkpoint derived from the facts above (an observation, not the controller's own): %s\n", f.checkpoint())
	return b.String()
}

// checkpoint names the stage these facts amount to, so a report can be read without reconstructing the
// state machine by hand. It is derived from the objects only — never from status.cleanupReason or the Ready
// reason, which are the very things a failure puts in doubt.
func (f *recoveryFacts) checkpoint() string {
	prefix := ""
	if f.deleting {
		prefix = "Deleting + "
	}

	holderIsExportClaim := f.pv != nil && f.pv.Spec.ClaimRef != nil &&
		f.pv.Spec.ClaimRef.Namespace == d8DataManagerNS && f.pv.Spec.ClaimRef.Name == f.exportPVCName

	switch {
	case f.pv == nil:
		return prefix + "no volume readable"
	case !holderIsExportClaim && !f.recoveryPresent:
		return prefix + "NoExport/PreRebind: the volume is not held by an export claim and no takeover is recorded"
	case !holderIsExportClaim && f.recoveryPresent:
		return prefix + "PostRebind undone or lost to a stranger: a takeover is recorded but the volume is held elsewhere"
	case holderIsExportClaim && !f.recoveryPresent:
		return prefix + "legacy takeover: the volume is held by the export claim with no recorded identity"
	case f.exportClaim == nil:
		return prefix + "PostRebind, export claim gone: the volume is still bound to a claim that no longer exists"
	case f.pv.Spec.ClaimRef.UID != f.exportClaim.UID:
		return prefix + "PostRebind, export claim replaced: the live claim under that name does not hold the volume"
	case len(f.claimHolders) > 0:
		return prefix + "Serving"
	default:
		return prefix + "PostRebind, nothing serving it"
	}
}

func describeClaim(claim *corev1.PersistentVolumeClaim) string {
	if claim == nil {
		return "<absent>"
	}
	marker := claim.Annotations[sfDataExportUIDAnno]
	if marker == "" {
		marker = "<none>"
	}
	return fmt.Sprintf("phase=%s uid=%s volumeName=%q deleting=%v finalizers=%v %s=%s",
		claim.Status.Phase, claim.UID, claim.Spec.VolumeName, claim.DeletionTimestamp != nil, claim.Finalizers,
		sfDataExportUIDAnno, marker)
}

func describeVolume(pv *corev1.PersistentVolume) string {
	if pv == nil {
		return "<absent or unreadable>"
	}
	claimRef := "<none>"
	if pv.Spec.ClaimRef != nil {
		claimRef = fmt.Sprintf("%s/%s(uid=%s)", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name, pv.Spec.ClaimRef.UID)
	}
	marks := make([]string, 0, 6)
	for _, key := range []string{
		sfDataExportUIDAnno, sfUserPVCUIDAnno, sfOriginalPVCNameAnno, sfOriginalReclaimAnno,
		sfManagerNamespaceAnno, sfManagerNameAnno,
	} {
		if value := pv.Annotations[key]; value != "" {
			marks = append(marks, fmt.Sprintf("%s=%s", strings.TrimPrefix(key, "storage-foundation.deckhouse.io/"), value))
		}
	}
	if len(marks) == 0 {
		marks = append(marks, "<no export marks>")
	}
	return fmt.Sprintf("phase=%s reclaimPolicy=%s claimRef=%s\n    marks: %s",
		pv.Status.Phase, pv.Spec.PersistentVolumeReclaimPolicy, claimRef, strings.Join(marks, " "))
}
