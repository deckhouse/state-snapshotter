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
	"encoding/json"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// diagnosticsLogTailLines bounds the controller log tail dumped on failure.
const diagnosticsLogTailLines = 200

// dumpFailedSpecDiagnostics prints best-effort cluster state on a failed spec: ModuleConfig, the snapshot
// graph (Snapshots / SnapshotContents / demo snapshots) with their conditions, and the controller logs.
func dumpFailedSpecDiagnostics(ctx context.Context) {
	GinkgoWriter.Printf("\n========== state-snapshotter e2e diagnostics ==========\n")
	dumpModuleConfig(ctx)
	dumpResourceConditions(ctx, "Snapshot", snapshotGVR, true)
	if phase5ImportNS == "" {
		dumpResourceConditions(ctx, "SnapshotContent", snapshotContentGVR, false)
	}
	dumpResourceConditions(ctx, "DemoVirtualMachineSnapshot", demoVMSnapshotGVR, true)
	dumpResourceConditions(ctx, "DemoVirtualDiskSnapshot", demoDiskSnapshotGVR, true)
	dumpResourceConditions(ctx, "VolumeSnapshot", volumeSnapshotGVR, true)
	if phase5ImportNS != "" {
		dumpPhase5ImportDiagnostics(ctx, phase5ImportNS)
	}
	dumpControllerLogs(ctx, d8ModuleNS)
	dumpControllerLogs(ctx, d8SVDMModuleNS)
	// #region agent log
	dumpFilteredControllerLogs(ctx, d8ModuleNS, "DBGCAP")
	agentDebugDumpControllerLogsToFile(ctx, d8ModuleNS)
	// #endregion
	GinkgoWriter.Printf("=======================================================\n\n")
}

func dumpModuleConfig(ctx context.Context) {
	mc, err := suiteDyn.Resource(moduleConfigGVR).Get(ctx, moduleName, metav1.GetOptions{})
	if err != nil {
		GinkgoWriter.Printf("ModuleConfig %s: <get failed: %v>\n", moduleName, err)
		return
	}
	enabled, _, _ := unstructured.NestedBool(mc.Object, "spec", "enabled")
	settings, _, _ := unstructured.NestedMap(mc.Object, "spec", "settings")
	status, _, _ := unstructured.NestedString(mc.Object, "status", "status")
	GinkgoWriter.Printf("ModuleConfig %s: enabled=%v status=%q settings=%v\n", moduleName, enabled, status, settings)
}

// dumpResourceConditions lists a (possibly namespaced) resource across all namespaces and prints each
// object's conditions. namespaced=false addresses cluster-scoped kinds (SnapshotContent).
func dumpResourceConditions(ctx context.Context, label string, gvr schema.GroupVersionResource, namespaced bool) {
	var (
		list *unstructured.UnstructuredList
		err  error
	)
	if namespaced {
		list, err = suiteDyn.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	} else {
		list, err = suiteDyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		GinkgoWriter.Printf("%s: <list failed: %v>\n", label, err)
		return
	}
	if len(list.Items) == 0 {
		GinkgoWriter.Printf("%s: <none>\n", label)
		return
	}
	for i := range list.Items {
		obj := &list.Items[i]
		ref := obj.GetName()
		if ns := obj.GetNamespace(); ns != "" {
			ref = ns + "/" + ref
		}
		GinkgoWriter.Printf("%s %s:\n", label, ref)
		dumpObjectConditions(obj)
	}
}

func dumpObjectConditions(obj *unstructured.Unstructured) {
	conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !ok || len(conds) == 0 {
		GinkgoWriter.Printf("    <no conditions>\n")
		return
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		st, _, _ := unstructured.NestedString(m, "status")
		reason, _, _ := unstructured.NestedString(m, "reason")
		msg, _, _ := unstructured.NestedString(m, "message")
		GinkgoWriter.Printf("    %s=%s reason=%q msg=%q\n", t, st, reason, msg)
	}
}

func dumpPhase5ImportDiagnostics(ctx context.Context, importNS string) {
	GinkgoWriter.Printf("--- Phase 5 import namespace %s ---\n", importNS)
	dumpImportRootSnapshotContents(ctx, importNS, bkImportRootName)
	dumpDataImportsInNS(ctx, importNS)
	dumpImportLeavesInNS(ctx, importNS)
	dumpPVCsInNS(ctx, importNS)
	dumpPodsInNS(ctx, importNS)
	dumpFilteredControllerLogs(ctx, d8ModuleNS, importNS, "generic import", "DataImport", "Materialized import", "skipping")
	dumpFilteredControllerLogs(ctx, d8SVDMModuleNS, importNS, "DataImport", "dataimport", "Awaiting target leaf")
}

// dumpImportRootSnapshotContents prints SnapshotContent objects belonging to the import-root tree only
// (root content + direct children), omitting stale cluster-scoped duplicates from prior failed runs.
func dumpImportRootSnapshotContents(ctx context.Context, importNS, rootName string) {
	snap, err := getResource(ctx, snapshotGVR, importNS, rootName)
	if err != nil {
		GinkgoWriter.Printf("import-root Snapshot %s/%s: <get failed: %v>\n", importNS, rootName, err)
		return
	}
	rootContent, _, _ := unstructured.NestedString(snap.Object, "status", "boundSnapshotContentName")
	if rootContent == "" {
		GinkgoWriter.Printf("import-root SnapshotContent: <boundSnapshotContentName empty>\n")
		return
	}
	names := []string{rootContent}
	if co, gerr := getResource(ctx, snapshotContentGVR, "", rootContent); gerr == nil {
		names = append(names, childContentNames(co)...)
	}
	for _, name := range names {
		obj, gerr := getResource(ctx, snapshotContentGVR, "", name)
		if gerr != nil {
			GinkgoWriter.Printf("SnapshotContent %s: <get failed: %v>\n", name, gerr)
			continue
		}
		GinkgoWriter.Printf("SnapshotContent %s (import-root tree):\n", name)
		dumpObjectConditions(obj)
		mcp, _, _ := unstructured.NestedString(obj.Object, "status", "manifestCheckpointName")
		GinkgoWriter.Printf("    manifestCheckpointName=%q ownerRefs=%s\n", mcp, formatOwnerReferences(obj.GetOwnerReferences()))
	}
}

// dumpStuckDataImportDiagnostics prints a focused snapshot when waitDataImportReady times out.
func dumpStuckDataImportDiagnostics(ctx context.Context, importNS, diName string) {
	GinkgoWriter.Printf("\n--- DataImport stuck: %s/%s ---\n", importNS, diName)
	dumpSingleDataImport(ctx, importNS, diName)
	dumpImportLeavesInNS(ctx, importNS)
	dumpPVCsInNS(ctx, importNS)
	dumpPodsInNS(ctx, importNS)
}

func dumpDataImportsInNS(ctx context.Context, ns string) {
	list, err := suiteDyn.Resource(dataImportGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("DataImport in %s: <list failed: %v>\n", ns, err)
		return
	}
	if len(list.Items) == 0 {
		GinkgoWriter.Printf("DataImport in %s: <none>\n", ns)
		return
	}
	for i := range list.Items {
		dumpSingleDataImport(ctx, ns, list.Items[i].GetName())
	}
}

func dumpSingleDataImport(ctx context.Context, ns, name string) {
	obj, err := getResource(ctx, dataImportGVR, ns, name)
	if err != nil {
		GinkgoWriter.Printf("DataImport %s/%s: <get failed: %v>\n", ns, name, err)
		return
	}
	targetGroup, _, _ := unstructured.NestedString(obj.Object, "spec", "targetRef", "group")
	targetResource, _, _ := unstructured.NestedString(obj.Object, "spec", "targetRef", "resource")
	targetName, _, _ := unstructured.NestedString(obj.Object, "spec", "targetRef", "name")
	url, _, _ := unstructured.NestedString(obj.Object, "status", "url")
	volMode, _, _ := unstructured.NestedString(obj.Object, "status", "volumeMode")
	ca, _, _ := unstructured.NestedString(obj.Object, "status", "ca")
	artifact, _, _ := unstructured.NestedMap(obj.Object, "status", "dataArtifactRef")
	GinkgoWriter.Printf("DataImport %s/%s:\n", ns, name)
	GinkgoWriter.Printf("    targetRef: group=%q resource=%q name=%q\n", targetGroup, targetResource, targetName)
	GinkgoWriter.Printf("    status: url=%q volumeMode=%q ca=%t dataArtifactRef=%v\n", url, volMode, ca != "", artifact)
	dumpObjectConditions(obj)

	// Resolve target leaf boundSnapshotContentName when DataImport waits on it.
	if targetName == "" {
		return
	}
	gvr := schema.GroupVersionResource{Group: targetGroup, Version: "v1alpha1", Resource: targetResource}
	if targetGroup == "snapshot.storage.k8s.io" {
		gvr.Version = "v1"
	}
	leaf, lerr := getResource(ctx, gvr, ns, targetName)
	if lerr != nil {
		GinkgoWriter.Printf("    target leaf %s/%s: <get failed: %v>\n", ns, targetName, lerr)
		return
	}
	bound, _, _ := unstructured.NestedString(leaf.Object, "status", "boundSnapshotContentName")
	GinkgoWriter.Printf("    target leaf boundSnapshotContentName=%q ownerRefs=%s\n",
		bound, formatOwnerReferences(leaf.GetOwnerReferences()))
}

func dumpImportLeavesInNS(ctx context.Context, ns string) {
	for _, entry := range []struct {
		label string
		gvr   schema.GroupVersionResource
	}{
		{"DemoVirtualDiskSnapshot", demoDiskSnapshotGVR},
		{"VolumeSnapshot", volumeSnapshotGVR},
	} {
		list, err := suiteDyn.Resource(entry.gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			GinkgoWriter.Printf("%s in %s: <list failed: %v>\n", entry.label, ns, err)
			continue
		}
		if len(list.Items) == 0 {
			GinkgoWriter.Printf("%s in %s: <none>\n", entry.label, ns)
			continue
		}
		for i := range list.Items {
			obj := &list.Items[i]
			bound, _, _ := unstructured.NestedString(obj.Object, "status", "boundSnapshotContentName")
			dataImport, _, _ := unstructured.NestedString(obj.Object, "spec", "dataSource", "name")
			if dataImport == "" {
				dataImport, _, _ = unstructured.NestedString(obj.Object, "spec", "source", "dataImportName")
			}
			GinkgoWriter.Printf("%s %s/%s:\n", entry.label, ns, obj.GetName())
			GinkgoWriter.Printf("    boundSnapshotContentName=%q dataImport=%q ownerRefs=%s\n",
				bound, dataImport, formatOwnerReferences(obj.GetOwnerReferences()))
			dumpObjectConditions(obj)
		}
	}
}

func formatOwnerReferences(refs []metav1.OwnerReference) string {
	if len(refs) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		ctrl := ""
		if ref.Controller != nil && *ref.Controller {
			ctrl = ",controller"
		}
		parts = append(parts, fmt.Sprintf("%s/%s(uid=%s%s)", ref.Kind, ref.Name, ref.UID, ctrl))
	}
	return strings.Join(parts, "; ")
}

func dumpPVCsInNS(ctx context.Context, ns string) {
	pvcs, err := suiteClientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("PVC in %s: <list failed: %v>\n", ns, err)
		return
	}
	if len(pvcs.Items) == 0 {
		GinkgoWriter.Printf("PVC in %s: <none>\n", ns)
		return
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		GinkgoWriter.Printf("PVC %s/%s: phase=%s storageClass=%q volumeMode=%q volumeName=%q\n",
			ns, pvc.Name, pvc.Status.Phase, ptrStr(pvc.Spec.StorageClassName), ptrStr(pvc.Spec.VolumeMode), pvc.Spec.VolumeName)
	}
}

func dumpPodsInNS(ctx context.Context, ns string) {
	pods, err := suiteClientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("Pods in %s: <list failed: %v>\n", ns, err)
		return
	}
	if len(pods.Items) == 0 {
		GinkgoWriter.Printf("Pods in %s: <none>\n", ns)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		GinkgoWriter.Printf("Pod %s/%s: phase=%s ready=%v ownerRefs=%s\n",
			ns, pod.Name, pod.Status.Phase, podReady(pod), formatOwnerReferences(pod.OwnerReferences))
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				GinkgoWriter.Printf("    container %s waiting: reason=%q message=%q\n",
					cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func ptrStr[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func dumpControllerLogs(ctx context.Context, moduleNS string) {
	pods, err := suiteClientset.CoreV1().Pods(moduleNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("controller pods in %s: <list failed: %v>\n", moduleNS, err)
		return
	}
	tail := int64(diagnosticsLogTailLines)
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Spec.Containers {
			data, err := suiteClientset.CoreV1().Pods(moduleNS).
				GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name, TailLines: &tail}).
				DoRaw(ctx)
			if err != nil {
				GinkgoWriter.Printf("logs %s/%s [%s]: <failed: %v>\n", moduleNS, pod.Name, container.Name, err)
				continue
			}
			GinkgoWriter.Printf("---- logs %s/%s [%s] (last %d lines) ----\n%s\n",
				moduleNS, pod.Name, container.Name, tail, string(data))
		}
	}
}

// dumpFilteredControllerLogs prints log lines from moduleNS that match any of the substrings (case-sensitive).
func dumpFilteredControllerLogs(ctx context.Context, moduleNS string, filters ...string) {
	if len(filters) == 0 {
		return
	}
	pods, err := suiteClientset.CoreV1().Pods(moduleNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("filtered logs %s: <list pods failed: %v>\n", moduleNS, err)
		return
	}
	tail := int64(2000)
	const maxLines = 200
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Spec.Containers {
			data, err := suiteClientset.CoreV1().Pods(moduleNS).
				GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name, TailLines: &tail}).
				DoRaw(ctx)
			if err != nil {
				continue
			}
			var matched []string
			for _, line := range strings.Split(string(data), "\n") {
				for _, f := range filters {
					if strings.Contains(line, f) {
						matched = append(matched, line)
						break
					}
				}
			}
			if len(matched) == 0 {
				continue
			}
			if len(matched) > maxLines {
				matched = matched[len(matched)-maxLines:]
			}
			GinkgoWriter.Printf("---- filtered logs %s/%s [%s] (filters=%s, showing %d lines) ----\n%s\n",
				moduleNS, pod.Name, container.Name, jsonFilters(filters), len(matched), strings.Join(matched, "\n"))
		}
	}
}

func jsonFilters(filters []string) string {
	b, err := json.Marshal(filters)
	if err != nil {
		return fmt.Sprintf("%v", filters)
	}
	return string(b)
}
