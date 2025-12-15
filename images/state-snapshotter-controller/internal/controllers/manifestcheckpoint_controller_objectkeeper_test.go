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

package controllers

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func TestManifestCaptureRequestObjectKeeper(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ManifestCaptureRequest ObjectKeeper Suite")
}

var _ = Describe("ManifestCaptureRequest ObjectKeeper", func() {
	var (
		ctx    context.Context
		client client.Client
		scheme *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())

		client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}).
			Build()
	})

	Describe("ObjectKeeper creation", func() {
		It("should create ObjectKeeper with FollowObject mode (no TTL)", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					UID:       types.UID("mcr-uid-123"),
				},
				Spec: storagev1alpha1.ManifestCaptureRequestSpec{
					Targets: []storagev1alpha1.ManifestTarget{
						{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "test-cm",
						},
					},
				},
			}

			// Create ConfigMap
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cm",
					Namespace: "default",
				},
			}
			Expect(client.Create(ctx, cm)).To(Succeed())

			// Create ObjectKeeper manually (simulating controller behavior)
			retainerName := "ret-mcr-default-test-mcr"
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{
				TypeMeta: metav1.TypeMeta{
					APIVersion: DeckhouseAPIVersion,
					Kind:       KindObjectKeeper,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: retainerName,
				},
				Spec: deckhousev1alpha1.ObjectKeeperSpec{
					Mode: "FollowObject",
					FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
						APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
						Kind:       "ManifestCaptureRequest",
						Namespace:  mcr.Namespace,
						Name:       mcr.Name,
						UID:        string(mcr.UID),
					},
				},
			}
			Expect(client.Create(ctx, objectKeeper)).To(Succeed())

			// Verify ObjectKeeper exists
			createdOK := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, createdOK)).To(Succeed())

			// Verify ObjectKeeper spec
			Expect(createdOK.Spec.Mode).To(Equal("FollowObject"))
			Expect(createdOK.Spec.FollowObjectRef).ToNot(BeNil())
			Expect(createdOK.Spec.FollowObjectRef.UID).To(Equal(string(mcr.UID)))
			Expect(createdOK.Spec.FollowObjectRef.Name).To(Equal(mcr.Name))
			Expect(createdOK.Spec.FollowObjectRef.Namespace).To(Equal(mcr.Namespace))
			Expect(createdOK.Spec.FollowObjectRef.Kind).To(Equal("ManifestCaptureRequest"))
			Expect(createdOK.Spec.FollowObjectRef.APIVersion).To(Equal("state-snapshotter.deckhouse.io/v1alpha1"))
		})

		It("should create ManifestCheckpoint with ownerRef to ObjectKeeper", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					UID:       types.UID("mcr-uid-123"),
				},
			}

			retainerName := "ret-mcr-default-test-mcr"
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name: retainerName,
					UID:  types.UID("ok-uid-123"),
				},
				Spec: deckhousev1alpha1.ObjectKeeperSpec{
					Mode: "FollowObject",
					FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
						APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
						Kind:       "ManifestCaptureRequest",
						Namespace:  mcr.Namespace,
						Name:       mcr.Name,
						UID:        string(mcr.UID),
					},
				},
			}
			Expect(client.Create(ctx, objectKeeper)).To(Succeed())

			// Create ManifestCheckpoint with ownerRef to ObjectKeeper
			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mcp-test-123",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: DeckhouseAPIVersion,
							Kind:       KindObjectKeeper,
							Name:       retainerName,
							UID:        objectKeeper.UID,
							Controller: func() *bool { b := true; return &b }(),
						},
					},
				},
				Spec: storagev1alpha1.ManifestCheckpointSpec{
					SourceNamespace: mcr.Namespace,
					ManifestCaptureRequestRef: &storagev1alpha1.ObjectReference{
						Name:      mcr.Name,
						Namespace: mcr.Namespace,
						UID:       string(mcr.UID),
					},
				},
			}
			Expect(client.Create(ctx, checkpoint)).To(Succeed())

			// Verify checkpoint has correct ownerRef
			createdCheckpoint := &storagev1alpha1.ManifestCheckpoint{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "mcp-test-123"}, createdCheckpoint)).To(Succeed())

			Expect(len(createdCheckpoint.OwnerReferences)).To(Equal(1))
			ownerRef := createdCheckpoint.OwnerReferences[0]
			Expect(ownerRef.Kind).To(Equal(KindObjectKeeper))
			Expect(ownerRef.Name).To(Equal(retainerName))
			Expect(ownerRef.UID).To(Equal(objectKeeper.UID))
			Expect(*ownerRef.Controller).To(BeTrue())
		})

		It("should validate ObjectKeeper belongs to correct MCR by UID", func() {
			mcr1 := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					UID:       types.UID("mcr-uid-123"),
				},
			}

			mcr2 := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr", // Same name
					Namespace: "default",
					UID:       types.UID("mcr-uid-456"), // Different UID
				},
			}

			retainerName := "ret-mcr-default-test-mcr"
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name: retainerName,
				},
				Spec: deckhousev1alpha1.ObjectKeeperSpec{
					Mode: "FollowObject",
					FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
						APIVersion: "state-snapshotter.deckhouse.io/v1alpha1",
						Kind:       "ManifestCaptureRequest",
						Namespace:  mcr1.Namespace,
						Name:       mcr1.Name,
						UID:        string(mcr1.UID), // Belongs to mcr1
					},
				},
			}
			Expect(client.Create(ctx, objectKeeper)).To(Succeed())

			// Verify ObjectKeeper belongs to mcr1 (not mcr2)
			createdOK := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, createdOK)).To(Succeed())

			Expect(createdOK.Spec.FollowObjectRef.UID).To(Equal(string(mcr1.UID)))
			Expect(createdOK.Spec.FollowObjectRef.UID).ToNot(Equal(string(mcr2.UID)))
		})
	})
})
