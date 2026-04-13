//go:build integration
// +build integration

/*
Copyright 2025 Flant JSC

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

package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/api"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase/restore"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func pr4IntegrationEncodeChunk(objects []map[string]interface{}) (data string, checksum string) {
	jsonData, _ := json.Marshal(objects)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(jsonData)
	_ = gz.Close()
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	hash := sha256.Sum256(buf.Bytes())
	return encoded, hex.EncodeToString(hash[:])
}

// pr4IntegrationMustInstallReadyMCP creates chunk + ManifestCheckpoint and writes MCP status (Create ignores .Status).
func pr4IntegrationMustInstallReadyMCP(ctx context.Context, cl client.Client, name, srcNS string, objects []map[string]interface{}) *ssv1alpha1.ManifestCheckpoint {
	d, cs := pr4IntegrationEncodeChunk(objects)
	chName := name + "-chunk-0"
	ch := &ssv1alpha1.ManifestCheckpointContentChunk{
		ObjectMeta: metav1.ObjectMeta{Name: chName},
		Spec: ssv1alpha1.ManifestCheckpointContentChunkSpec{
			CheckpointName: name,
			Index:          0,
			Data:           d,
			Checksum:       cs,
			ObjectsCount:   len(objects),
		},
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: ssv1alpha1.ManifestCheckpointSpec{
			SourceNamespace: srcNS,
			ManifestCaptureRequestRef: &ssv1alpha1.ObjectReference{
				Name:      "mcr-pr4-" + name,
				Namespace: srcNS,
				UID:       "pr4-mcr-uid-" + name,
			},
		},
	}
	Expect(cl.Create(ctx, ch)).To(Succeed())
	Expect(cl.Create(ctx, mcp)).To(Succeed())
	mcp.Status = ssv1alpha1.ManifestCheckpointStatus{
		Chunks:       []ssv1alpha1.ChunkInfo{{Name: chName, Index: 0, Checksum: cs}},
		TotalObjects: len(objects),
	}
	meta.SetStatusCondition(&mcp.Status.Conditions, metav1.Condition{
		Type: ssv1alpha1.ManifestCheckpointConditionTypeReady, Status: metav1.ConditionTrue,
		Reason: ssv1alpha1.ManifestCheckpointConditionReasonCompleted,
	})
	Expect(cl.Status().Update(ctx, mcp)).To(Succeed())
	return mcp
}

// pr4IntegrationMustCreateNSC creates a NamespaceSnapshotContent and writes status (Create ignores .Status on CRD).
func pr4IntegrationMustCreateNSC(ctx context.Context, cl client.Client, name, snapNS, snapName, mcpName string, children ...string) {
	var refs []storagev1alpha1.NamespaceSnapshotContentChildRef
	for _, c := range children {
		refs = append(refs, storagev1alpha1.NamespaceSnapshotContentChildRef{Name: c})
	}
	nsc := &storagev1alpha1.NamespaceSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1alpha1.NamespaceSnapshotContentSpec{
			NamespaceSnapshotRef: storagev1alpha1.SnapshotSubjectRef{
				APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
				Kind:       "NamespaceSnapshot",
				Name:       snapName,
				Namespace:  snapNS,
			},
			DeletionPolicy: storagev1alpha1.SnapshotContentDeletionPolicyRetain,
		},
	}
	Expect(cl.Create(ctx, nsc)).To(Succeed())
	nsc.Status.ManifestCheckpointName = mcpName
	nsc.Status.ChildrenSnapshotContentRefs = refs
	meta.SetStatusCondition(&nsc.Status.Conditions, metav1.Condition{
		Type: snapshot.ConditionReady, Status: metav1.ConditionTrue, Reason: "Completed",
	})
	Expect(cl.Status().Update(ctx, nsc)).To(Succeed())
}

func pr4IntegrationStartServer() *httptest.Server {
	log, err := logger.NewLogger("error")
	Expect(err).NotTo(HaveOccurred())
	arch := usecase.NewArchiveService(k8sClient, k8sClient, log)
	agg := usecase.NewAggregatedNamespaceManifests(k8sClient, arch)
	ah := api.NewArchiveHandler(k8sClient, arch, log)
	rs := restore.NewService(k8sClient, arch)
	rh := api.NewRestoreHandler(k8sClient, rs, log, agg)
	mux := http.NewServeMux()
	ah.SetupRoutes(mux)
	rh.SetupRoutes(mux)
	return httptest.NewServer(mux)
}

var _ = Describe("Integration: NamespaceSnapshot PR4 aggregated manifests", func() {
	It("returns aggregated manifests for parent-only (N2a lifecycle)", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-pr4-",
				Labels:       map[string]string{"state-snapshotter.deckhouse.io/test": "nss-pr4"},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: nsName}, Data: map[string]string{"k": "v"}}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snap := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		Eventually(func(g Gomega) {
			f := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "snap"}, f)).To(Succeed())
			rc := meta.FindStatusCondition(f.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(f.Status.BoundSnapshotContentName).NotTo(BeEmpty())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		srv := pr4IntegrationStartServer()
		defer srv.Close()
		url := fmt.Sprintf("%s/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/%s/namespacesnapshots/snap/manifests", srv.URL, nsName)
		resp, err := http.Get(url)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		var arr []map[string]interface{}
		Expect(json.Unmarshal(body, &arr)).To(Succeed())
		Expect(arr).NotTo(BeEmpty())
	})

	It("returns aggregated manifests for parent + one manual child NSC (disjoint MCP objects)", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-pr4-tree-",
				Labels:       map[string]string{"state-snapshotter.deckhouse.io/test": "nss-pr4-tree"},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: nsName}, Data: map[string]string{"k": "v"}}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snap := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		var rootContentName string
		Eventually(func(g Gomega) {
			f := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "snap"}, f)).To(Succeed())
			rc := meta.FindStatusCondition(f.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			rootContentName = f.Status.BoundSnapshotContentName
			g.Expect(rootContentName).NotTo(BeEmpty())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		child := "pr4-extr-one-" + nsName
		mcpChild := pr4IntegrationMustInstallReadyMCP(ctx, k8sClient, "mcp-pr4-one-"+nsName, nsName, []map[string]interface{}{
			{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "only-child", "namespace": nsName}},
		})
		pr4IntegrationMustCreateNSC(ctx, k8sClient, child, nsName, "snap", mcpChild.Name)

		rootNSC := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: rootContentName}, rootNSC)).To(Succeed())
		base := rootNSC.DeepCopy()
		rootNSC.Status.ChildrenSnapshotContentRefs = []storagev1alpha1.NamespaceSnapshotContentChildRef{{Name: child}}
		Expect(k8sClient.Status().Patch(ctx, rootNSC, client.MergeFrom(base))).To(Succeed())

		srv := pr4IntegrationStartServer()
		defer srv.Close()
		url := fmt.Sprintf("%s/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/%s/namespacesnapshots/snap/manifests", srv.URL, nsName)
		resp, err := http.Get(url)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		var arr []map[string]interface{}
		Expect(json.NewDecoder(resp.Body).Decode(&arr)).To(Succeed())
		Expect(len(arr)).To(BeNumerically(">=", 2))
		foundCM, foundSecret := false, false
		for _, o := range arr {
			k, _ := o["kind"].(string)
			m, ok := o["metadata"].(map[string]interface{})
			if !ok {
				continue
			}
			n, _ := m["name"].(string)
			if k == "ConfigMap" && n == "cm1" {
				foundCM = true
			}
			if k == "Secret" && n == "only-child" {
				foundSecret = true
			}
		}
		Expect(foundCM).To(BeTrue(), "root MCP should include cm1")
		Expect(foundSecret).To(BeTrue(), "child MCP should include only-child")
	})

	It("aggregates parent + two manual child NSCs (lexicographic child order)", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nss-pr4-2ch-",
				Labels:       map[string]string{"state-snapshotter.deckhouse.io/test": "nss-pr4-2ch"},
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		nsName := ns.Name
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}) })

		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: nsName}, Data: map[string]string{"k": "v"}}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		snap := &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: nsName},
			Spec:       storagev1alpha1.NamespaceSnapshotSpec{},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		var rootContentName string
		Eventually(func(g Gomega) {
			f := &storagev1alpha1.NamespaceSnapshot{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "snap"}, f)).To(Succeed())
			rc := meta.FindStatusCondition(f.Status.Conditions, snapshot.ConditionReady)
			g.Expect(rc).NotTo(BeNil())
			g.Expect(rc.Status).To(Equal(metav1.ConditionTrue))
			rootContentName = f.Status.BoundSnapshotContentName
			g.Expect(rootContentName).NotTo(BeEmpty())
		}).WithTimeout(90 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

		childA := "pr4-extr-a-" + nsName
		childB := "pr4-extr-b-" + nsName

		mcpA := pr4IntegrationMustInstallReadyMCP(ctx, k8sClient, "mcp-pr4-a-"+nsName, nsName, []map[string]interface{}{
			{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "only-a", "namespace": nsName}},
		})
		mcpB := pr4IntegrationMustInstallReadyMCP(ctx, k8sClient, "mcp-pr4-b-"+nsName, nsName, []map[string]interface{}{
			{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]interface{}{"name": "only-b", "namespace": nsName}},
		})

		pr4IntegrationMustCreateNSC(ctx, k8sClient, childA, nsName, "snap", mcpA.Name)
		pr4IntegrationMustCreateNSC(ctx, k8sClient, childB, nsName, "snap", mcpB.Name)

		rootNSC := &storagev1alpha1.NamespaceSnapshotContent{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: rootContentName}, rootNSC)).To(Succeed())
		base := rootNSC.DeepCopy()
		// Unsorted refs — aggregator must sort by name
		rootNSC.Status.ChildrenSnapshotContentRefs = []storagev1alpha1.NamespaceSnapshotContentChildRef{
			{Name: childB},
			{Name: childA},
		}
		Expect(k8sClient.Status().Patch(ctx, rootNSC, client.MergeFrom(base))).To(Succeed())

		srv := pr4IntegrationStartServer()
		defer srv.Close()
		url := fmt.Sprintf("%s/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/%s/namespacesnapshots/snap/manifests", srv.URL, nsName)
		resp, err := http.Get(url)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		var arr []map[string]interface{}
		Expect(json.NewDecoder(resp.Body).Decode(&arr)).To(Succeed())

		var names []string
		for _, o := range arr {
			m := o["metadata"].(map[string]interface{})
			names = append(names, m["name"].(string))
		}
		ia, ib := -1, -1
		for i, n := range names {
			if n == "only-a" {
				ia = i
			}
			if n == "only-b" {
				ib = i
			}
		}
		Expect(ia).To(BeNumerically(">", -1))
		Expect(ib).To(BeNumerically(">", -1))
		Expect(ia).To(BeNumerically("<", ib), "child-a before child-b lexicographically")
	})

	It("returns 404 for missing NamespaceSnapshot", func() {
		srv := pr4IntegrationStartServer()
		defer srv.Close()
		url := srv.URL + "/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/namespaces/default/namespacesnapshots/does-not-exist-pr4/manifests"
		resp, err := http.Get(url)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})
})
