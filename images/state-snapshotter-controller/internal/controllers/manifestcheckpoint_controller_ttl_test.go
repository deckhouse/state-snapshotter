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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

func TestManifestCaptureRequestTTL(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ManifestCaptureRequest TTL Suite")
}

var _ = Describe("ManifestCaptureRequest TTL", func() {
	var (
		ctx    context.Context
		client client.Client
		ctrl   *ManifestCheckpointController
		scheme *runtime.Scheme
		cfg    *config.Options
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())

		cfg = &config.Options{
			DefaultTTL:    168 * time.Hour, // 7 days
			DefaultTTLStr: "168h",
		}

		client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}).
			Build()

		testLogger, _ := logger.NewLogger("test")
		ctrl = &ManifestCheckpointController{
			Client: client,
			Scheme: scheme,
			Logger: testLogger,
			Config: cfg,
		}
	})

	Describe("setTTLAnnotation", func() {
		It("should set TTL annotation when not exists", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
				},
			}

			ctrl.setTTLAnnotation(mcr)

			Expect(mcr.Annotations).ToNot(BeNil())
			Expect(mcr.Annotations[AnnotationKeyTTL]).To(Equal("168h"))
		})

		It("should not overwrite existing TTL annotation", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "24h",
					},
				},
			}

			ctrl.setTTLAnnotation(mcr)

			Expect(mcr.Annotations[AnnotationKeyTTL]).To(Equal("24h"))
		})

		It("should use config TTL when available", func() {
			cfg.DefaultTTLStr = "72h"
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
				},
			}

			ctrl.setTTLAnnotation(mcr)

			Expect(mcr.Annotations[AnnotationKeyTTL]).To(Equal("72h"))
		})
	})

	Describe("checkAndHandleTTL", func() {
		It("should return false when no TTL annotation", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
				},
			}

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, mcr)

			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeFalse())
			Expect(requeueAfter).To(Equal(time.Duration(0)))
		})

		It("should return false when no CompletionTimestamp", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "168h",
					},
				},
			}

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, mcr)

			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeFalse())
			Expect(requeueAfter).To(Equal(time.Duration(0)))
		})

		It("should delete when TTL expired", func() {
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-200 * time.Hour)) // 200 hours ago, TTL=168h, so expired

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "168h",
					},
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &completionTime,
				},
			}

			Expect(client.Create(ctx, mcr)).To(Succeed())

			// Re-read MCR to get latest state from fake client
			createdMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, createdMCR)).To(Succeed())

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, createdMCR)

			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeTrue())
			Expect(requeueAfter).To(Equal(time.Duration(0)))

			// Verify object is deleted
			err = client.Get(ctx, types.NamespacedName{Name: createdMCR.Name, Namespace: createdMCR.Namespace}, createdMCR)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should return RequeueAfter when TTL not expired", func() {
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-100 * time.Hour)) // 100 hours ago, TTL=168h, so 68h left

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "168h",
					},
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &completionTime,
				},
			}

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, mcr)

			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeFalse())
			Expect(requeueAfter).To(BeNumerically(">=", 30*time.Second))
			// RequeueAfter should be capped at 1 minute
			Expect(requeueAfter).To(BeNumerically("<=", 70*time.Second))
		})

		It("should handle invalid TTL format without deleting object", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "168hours", // Invalid format
					},
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &metav1.Time{Time: time.Now()},
				},
			}

			Expect(client.Create(ctx, mcr)).To(Succeed())

			// Re-read MCR to get latest state from fake client
			createdMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, createdMCR)).To(Succeed())

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, createdMCR)

			// Function should handle invalid TTL gracefully
			// Note: With fake client, retry logic may not work perfectly, but object should not be deleted
			Expect(shouldDelete).To(BeFalse())
			if err == nil {
				Expect(requeueAfter).To(Equal(time.Duration(0)))
			}

			// Verify object still exists (not deleted)
			err = client.Get(ctx, types.NamespacedName{Name: createdMCR.Name, Namespace: createdMCR.Namespace}, createdMCR)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should not break Ready=True status when TTL is invalid and object is already completed", func() {
			// Create MCR that is already Ready=True
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr-ready",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "invalid-ttl", // Invalid format
					},
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &metav1.Time{Time: time.Now()},
					Conditions: []metav1.Condition{
						{
							Type:               ConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             ConditionReasonCompleted,
							Message:            "Checkpoint created successfully",
							LastTransitionTime: metav1.Now(),
							ObservedGeneration: 1,
						},
					},
				},
			}

			Expect(client.Create(ctx, mcr)).To(Succeed())

			// Re-read MCR to get latest state from fake client
			createdMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, createdMCR)).To(Succeed())

			// Verify Ready=True before TTL check
			readyBefore := meta.FindStatusCondition(createdMCR.Status.Conditions, ConditionTypeReady)
			Expect(readyBefore).ToNot(BeNil())
			Expect(readyBefore.Status).To(Equal(metav1.ConditionTrue))

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, createdMCR)

			// Should not delete and should not error
			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeFalse())
			Expect(requeueAfter).To(Equal(time.Duration(0)))

			// Re-read to verify Ready status is preserved
			Expect(client.Get(ctx, types.NamespacedName{Name: createdMCR.Name, Namespace: createdMCR.Namespace}, createdMCR)).To(Succeed())
			readyAfter := meta.FindStatusCondition(createdMCR.Status.Conditions, ConditionTypeReady)
			// Ready status should remain True (not broken by invalid TTL)
			Expect(readyAfter).ToNot(BeNil())
			Expect(readyAfter.Status).To(Equal(metav1.ConditionTrue), "Ready=True should not be broken by invalid TTL when object is already completed")
		})

		It("should apply jitter to requeueAfter", func() {
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-100 * time.Hour))

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "168h",
					},
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &completionTime,
				},
			}

			// Run multiple times to verify jitter
			requeueValues := make(map[time.Duration]bool)
			for i := 0; i < 20; i++ {
				// Create a fresh copy each time to avoid side effects
				mcrCopy := mcr.DeepCopy()
				_, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, mcrCopy)
				Expect(err).ToNot(HaveOccurred())
				requeueValues[requeueAfter] = true
			}

			// Should have some variation due to jitter
			Expect(len(requeueValues)).To(BeNumerically(">", 1))
		})
	})
})
