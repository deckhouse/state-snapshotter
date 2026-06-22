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
	dumpResourceConditions(ctx, "SnapshotContent", snapshotContentGVR, false)
	dumpResourceConditions(ctx, "DemoVirtualMachineSnapshot", demoVMSnapshotGVR, true)
	dumpResourceConditions(ctx, "DemoVirtualDiskSnapshot", demoDiskSnapshotGVR, true)
	dumpControllerLogs(ctx)
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
		conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !ok || len(conds) == 0 {
			GinkgoWriter.Printf("    <no conditions>\n")
			continue
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
}

func dumpControllerLogs(ctx context.Context) {
	pods, err := suiteClientset.CoreV1().Pods(d8ModuleNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("controller pods in %s: <list failed: %v>\n", d8ModuleNS, err)
		return
	}
	tail := int64(diagnosticsLogTailLines)
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Spec.Containers {
			data, err := suiteClientset.CoreV1().Pods(d8ModuleNS).
				GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name, TailLines: &tail}).
				DoRaw(ctx)
			if err != nil {
				GinkgoWriter.Printf("logs %s/%s [%s]: <failed: %v>\n", d8ModuleNS, pod.Name, container.Name, err)
				continue
			}
			GinkgoWriter.Printf("---- logs %s/%s [%s] (last %d lines) ----\n%s\n", d8ModuleNS, pod.Name, container.Name, tail, string(data))
		}
	}
}
