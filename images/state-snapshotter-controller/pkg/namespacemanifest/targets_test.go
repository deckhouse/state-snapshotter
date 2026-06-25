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

package namespacemanifest

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// fakeDiscovery is a minimal discovery.DiscoveryInterface double: only ServerPreferredNamespacedResources
// is exercised by BuildManifestCaptureTargets. The embedded interface is nil; any other method would panic
// (none are called here).
type fakeDiscovery struct {
	discovery.DiscoveryInterface
	resources []*metav1.APIResourceList
	err       error
}

func (f *fakeDiscovery) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return f.resources, f.err
}

// gvrEntry pairs a GVR with its list Kind for wiring both discovery and the dynamic fake.
type gvrEntry struct {
	gvr      schema.GroupVersionResource
	kind     string
	listKind string
}

func defaultGVRs() []gvrEntry {
	return []gvrEntry{
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}, "Pod", "PodList"},
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, "ConfigMap", "ConfigMapList"},
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}, "Secret", "SecretList"},
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}, "ServiceAccount", "ServiceAccountList"},
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "endpoints"}, "Endpoints", "EndpointsList"},
		{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}, "PersistentVolumeClaim", "PersistentVolumeClaimList"},
		{schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}, "Lease", "LeaseList"},
		{schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumendpoints"}, "CiliumEndpoint", "CiliumEndpointList"},
		{schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}, "VolumeSnapshot", "VolumeSnapshotList"},
		{schema.GroupVersionResource{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Resource: "manifestcapturerequests"}, "ManifestCaptureRequest", "ManifestCaptureRequestList"},
		{schema.GroupVersionResource{Group: "kafka.example.com", Version: "v1", Resource: "kafkas"}, "Kafka", "KafkaList"},
	}
}

func discoveryFromEntries(entries []gvrEntry, err error) *fakeDiscovery {
	byGV := map[string]*metav1.APIResourceList{}
	for _, e := range entries {
		gv := e.gvr.GroupVersion().String()
		list, ok := byGV[gv]
		if !ok {
			list = &metav1.APIResourceList{GroupVersion: gv}
			byGV[gv] = list
		}
		list.APIResources = append(list.APIResources, metav1.APIResource{
			Name:       e.gvr.Resource,
			Namespaced: true,
			Kind:       e.kind,
			Verbs:      metav1.Verbs{"get", "list", "watch"},
		})
	}
	out := make([]*metav1.APIResourceList, 0, len(byGV))
	for _, l := range byGV {
		out = append(out, l)
	}
	return &fakeDiscovery{resources: out, err: err}
}

func dynamicFromEntries(entries []gvrEntry, objs ...k8sruntime.Object) *fake.FakeDynamicClient {
	listKinds := make(map[schema.GroupVersionResource]string, len(entries))
	for _, e := range entries {
		listKinds[e.gvr] = e.listKind
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), listKinds, objs...)
}

func obj(apiVersion, kind, name string, mutate func(map[string]interface{})) *unstructured.Unstructured {
	meta := map[string]interface{}{"name": name, "namespace": "ns1"}
	o := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   meta,
	}}
	if mutate != nil {
		mutate(o.Object)
	}
	return o
}

func controllerOwnerRef() []interface{} {
	return []interface{}{map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "ReplicaSet",
		"name":       "rs",
		"uid":        "rs-uid",
		"controller": true,
	}}
}

func targetNames(targets []ManifestTarget) map[string]struct{} {
	out := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		out[t.Kind+"/"+t.Name] = struct{}{}
	}
	return out
}

func TestBuildManifestCaptureTargets_EmptyNamespaceHasNoTargets(t *testing.T) {
	entries := defaultGVRs()
	targets, unreadable, err := BuildManifestCaptureTargets(
		context.Background(),
		dynamicFromEntries(entries),
		discoveryFromEntries(entries, nil),
		"ns1",
		nil,
	)
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	if len(unreadable) != 0 {
		t.Fatalf("expected no unreadable GVRs, got %#v", unreadable)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets in empty namespace, got %#v", targets)
	}
}

func TestBuildManifestCaptureTargets_InclusionAndExclusionRules(t *testing.T) {
	entries := defaultGVRs()

	userCM := obj("v1", "ConfigMap", "app-config", nil)
	rootCA := obj("v1", "ConfigMap", "kube-root-ca.crt", nil)
	defaultSA := obj("v1", "ServiceAccount", "default", nil)
	userSA := obj("v1", "ServiceAccount", "app", nil)
	opaqueSecret := obj("v1", "Secret", "app-secret", func(o map[string]interface{}) { o["type"] = "Opaque" })
	tokenSecret := obj("v1", "Secret", "app-token", func(o map[string]interface{}) { o["type"] = "kubernetes.io/service-account-token" })
	standalonePod := obj("v1", "Pod", "standalone", nil)
	ownedPod := obj("v1", "Pod", "owned", func(o map[string]interface{}) {
		o["metadata"].(map[string]interface{})["ownerReferences"] = controllerOwnerRef()
	})
	endpoints := obj("v1", "Endpoints", "app", nil)
	lease := obj("coordination.k8s.io/v1", "Lease", "leader", nil)
	ciliumEndpoint := obj("cilium.io/v2", "CiliumEndpoint", "app-pod-xyz", nil)
	csiVS := obj("snapshot.storage.k8s.io/v1", "VolumeSnapshot", "vs-a", nil)
	mcr := obj("state-snapshotter.deckhouse.io/v1alpha1", "ManifestCaptureRequest", "root-mcr", nil)
	kafka := obj("kafka.example.com/v1", "Kafka", "my-kafka", nil)

	dyn := dynamicFromEntries(entries,
		userCM, rootCA, defaultSA, userSA, opaqueSecret, tokenSecret,
		standalonePod, ownedPod, endpoints, lease, ciliumEndpoint, csiVS, mcr, kafka,
	)

	targets, unreadable, err := BuildManifestCaptureTargets(
		context.Background(),
		dyn,
		discoveryFromEntries(entries, nil),
		"ns1",
		nil,
	)
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	if len(unreadable) != 0 {
		t.Fatalf("expected no unreadable GVRs, got %#v", unreadable)
	}

	got := targetNames(targets)
	mustInclude := []string{
		"ConfigMap/app-config",
		"ServiceAccount/app",
		"Secret/app-secret",
		"Pod/standalone",
		"Kafka/my-kafka", // arbitrary CR with no CSD mapping — the headline feature
	}
	for _, k := range mustInclude {
		if _, ok := got[k]; !ok {
			t.Errorf("expected %q to be included, targets=%v", k, got)
		}
	}
	mustExclude := []string{
		"ConfigMap/kube-root-ca.crt",
		"ServiceAccount/default",
		"Secret/app-token",
		"Pod/owned",
		"Endpoints/app",
		"Lease/leader",
		"CiliumEndpoint/app-pod-xyz",
		"VolumeSnapshot/vs-a",
		"ManifestCaptureRequest/root-mcr",
	}
	for _, k := range mustExclude {
		if _, ok := got[k]; ok {
			t.Errorf("expected %q to be excluded, targets=%v", k, got)
		}
	}
}

func TestBuildManifestCaptureTargets_ExcludesRegisteredSnapshotKinds(t *testing.T) {
	fooSnapGVR := schema.GroupVersionResource{Group: "x.example.com", Version: "v1", Resource: "foosnapshots"}
	entries := append(defaultGVRs(), gvrEntry{fooSnapGVR, "FooSnapshot", "FooSnapshotList"})

	fooSnap := obj("x.example.com/v1", "FooSnapshot", "snap-1", nil)
	dyn := dynamicFromEntries(entries, fooSnap)

	snapshotKinds := SnapshotMachineryGVKs{
		{Group: "x.example.com", Version: "v1", Kind: "FooSnapshot"}: {},
	}
	targets, _, err := BuildManifestCaptureTargets(
		context.Background(),
		dyn,
		discoveryFromEntries(entries, nil),
		"ns1",
		snapshotKinds,
	)
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets: %v", err)
	}
	if _, ok := targetNames(targets)["FooSnapshot/snap-1"]; ok {
		t.Fatalf("registered snapshot kind must be excluded (mechanism 1), got %#v", targets)
	}
}

func TestBuildManifestCaptureTargets_ForbiddenListGoesToUnreadable(t *testing.T) {
	entries := defaultGVRs()
	kafka := obj("kafka.example.com/v1", "Kafka", "my-kafka", nil)
	dyn := dynamicFromEntries(entries, kafka)
	dyn.PrependReactor("list", "kafkas", func(clienttesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kafka.example.com", Resource: "kafkas"}, "", errors.New("rbac not ready"))
	})

	targets, unreadable, err := BuildManifestCaptureTargets(
		context.Background(),
		dyn,
		discoveryFromEntries(entries, nil),
		"ns1",
		nil,
	)
	if err != nil {
		t.Fatalf("BuildManifestCaptureTargets must not return hard error on Forbidden, got %v", err)
	}
	if _, ok := targetNames(targets)["Kafka/my-kafka"]; ok {
		t.Fatalf("forbidden-listed Kafka must not appear in targets, got %#v", targets)
	}
	var found bool
	for _, gvr := range unreadable {
		if gvr.Resource == "kafkas" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected kafkas GVR in unreadable, got %#v", unreadable)
	}
}

func TestBuildManifestCaptureTargets_PartialDiscoveryGoesToUnreadable(t *testing.T) {
	entries := defaultGVRs()
	brokenGV := schema.GroupVersion{Group: "broken.example.com", Version: "v1"}
	groupErr := &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{brokenGV: errors.New("aggregated apiservice down")}}

	targets, unreadable, err := BuildManifestCaptureTargets(
		context.Background(),
		dynamicFromEntries(entries),
		discoveryFromEntries(entries, groupErr),
		"ns1",
		nil,
	)
	if err != nil {
		t.Fatalf("partial discovery must not be a hard error, got %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected no targets, got %#v", targets)
	}
	var found bool
	for _, gvr := range unreadable {
		if gvr.Group == "broken.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected broken group in unreadable, got %#v", unreadable)
	}
}

func TestBuildManifestCaptureTargets_ListErrorIsReturnedWrapped(t *testing.T) {
	entries := defaultGVRs()
	dyn := dynamicFromEntries(entries)
	sentinel := apierrors.NewServiceUnavailable("apiserver hiccup")
	dyn.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, sentinel
	})

	_, _, err := BuildManifestCaptureTargets(
		context.Background(),
		dyn,
		discoveryFromEntries(entries, nil),
		"ns1",
		nil,
	)
	if err == nil {
		t.Fatal("expected a wrapped error for a non-Forbidden list failure")
	}
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("expected wrapped error to preserve apierrors classification (ServiceUnavailable), got %v", err)
	}
}

func TestShouldIncludeNamespaceObject_DefaultInclude(t *testing.T) {
	if !ShouldIncludeNamespaceObject(obj("v1", "ConfigMap", "x", nil), nil) {
		t.Fatal("plain ConfigMap with no exclusion signal must be included")
	}
}
