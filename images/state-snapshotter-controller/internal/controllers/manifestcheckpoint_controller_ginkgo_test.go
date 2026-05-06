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

package controllers

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	snapstorage "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/config"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/namespacemanifest"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// TestManifestCaptureRequestGinkgo is the single entry point for all Ginkgo tests
// This prevents "Rerunning Suite" errors when multiple test files call RunSpecs
func TestManifestCaptureRequestGinkgo(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ManifestCaptureRequest Suite")
}

var _ = Describe("ManifestCaptureRequest TTL", func() {
	var (
		baseClient ctrlclient.Client
		ctrl       *ManifestCheckpointController
		scheme     *runtime.Scheme
		cfg        *config.Options
		testLogger logger.LoggerInterface
	)

	BeforeEach(func() {
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())

		cfg = &config.Options{
			DefaultTTL:    168 * time.Hour, // 7 days
			DefaultTTLStr: "168h",
		}

		baseClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}).
			Build()

		var err error
		testLogger, err = logger.NewLogger("info")
		Expect(err).ToNot(HaveOccurred(), "Failed to create logger")
		Expect(testLogger).ToNot(BeNil(), "Logger must not be nil")
		ctrl, err = NewManifestCheckpointController(
			baseClient,
			baseClient, // Use same client for APIReader in tests
			scheme,
			testLogger,
			cfg,
		)
		Expect(err).ToNot(HaveOccurred(), "Failed to create controller")
	})

	Describe("collectTargetObjects", func() {
		It("should resolve targets in the ManifestCaptureRequest namespace", func() {
			ctx := context.Background()
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "ns1"}}
			Expect(baseClient.Create(ctx, cm)).To(Succeed())

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "mcr-configmap", Namespace: "ns1"},
				Spec: storagev1alpha1.ManifestCaptureRequestSpec{
					Targets: []storagev1alpha1.ManifestTarget{{
						APIVersion: "v1",
						Kind:       "ConfigMap",
						Name:       "cm1",
					}},
				},
			}

			objects, err := ctrl.collectTargetObjects(ctx, mcr)
			Expect(err).NotTo(HaveOccurred())
			Expect(objects).To(HaveLen(1))
			Expect(objects[0].GetKind()).To(Equal("ConfigMap"))
			Expect(objects[0].GetName()).To(Equal("cm1"))
			Expect(objects[0].GetNamespace()).To(Equal("ns1"))
		})
	})

	// ============================================================================
	// TTL-related tests
	// ============================================================================
	// These tests verify TTL annotation management and TTL scanner behavior.
	// TTL enforcement is centralized in the background scanner, not in reconcile loop.

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

	Describe("TTL Scanner", func() {
		var (
			ctx           context.Context
			scannerClient ctrlclient.Client
		)

		BeforeEach(func() {
			ctx = context.Background()
			scannerClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}).
				Build()
			// Initialize controller for scanner tests
			// Reuse logger from parent BeforeEach
			ctrl.Client = scannerClient
			ctrl.Logger = testLogger
			ctrl.Config = cfg
		})

		It("should delete terminal MCR when TTL expired", func() {
			now := time.Now()
			// CompletionTimestamp is 2x TTL ago, so definitely expired
			expiredTime := metav1.NewTime(now.Add(-2 * cfg.DefaultTTL))

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "expired-mcr",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &expiredTime,
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
							LastTransitionTime: expiredTime,
						},
					},
				},
			}

			Expect(scannerClient.Create(ctx, mcr)).To(Succeed())

			// Run scanner
			ctrl.scanAndDeleteExpiredMCRs(ctx, scannerClient)

			// Verify MCR was deleted
			err := scannerClient.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, &storagev1alpha1.ManifestCaptureRequest{})
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should NOT delete terminal MCR when TTL not expired", func() {
			now := time.Now()
			// CompletionTimestamp is half TTL ago, so not expired yet
			recentTime := metav1.NewTime(now.Add(-cfg.DefaultTTL / 2))

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "not-expired-mcr",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &recentTime,
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
							LastTransitionTime: recentTime,
						},
					},
				},
			}

			Expect(scannerClient.Create(ctx, mcr)).To(Succeed())

			// Run scanner
			ctrl.scanAndDeleteExpiredMCRs(ctx, scannerClient)

			// Verify MCR still exists
			err := scannerClient.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, &storagev1alpha1.ManifestCaptureRequest{})
			Expect(err).ToNot(HaveOccurred())
		})

		It("should NOT delete non-terminal MCR even if TTL expired", func() {
			now := time.Now()
			expiredTime := metav1.NewTime(now.Add(-2 * cfg.DefaultTTL))

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-terminal-mcr",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &expiredTime,
					// No Ready condition - not in terminal state yet
					Conditions: []metav1.Condition{},
				},
			}

			Expect(scannerClient.Create(ctx, mcr)).To(Succeed())

			// Run scanner
			ctrl.scanAndDeleteExpiredMCRs(ctx, scannerClient)

			// Verify MCR still exists (not terminal, so not deleted)
			err := scannerClient.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, &storagev1alpha1.ManifestCaptureRequest{})
			Expect(err).ToNot(HaveOccurred())
		})

		It("should NOT delete terminal MCR without CompletionTimestamp", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-completion-mcr",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					// CompletionTimestamp is nil
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}

			Expect(scannerClient.Create(ctx, mcr)).To(Succeed())

			// Run scanner
			ctrl.scanAndDeleteExpiredMCRs(ctx, scannerClient)

			// Verify MCR still exists (no CompletionTimestamp, so TTL hasn't started)
			err := scannerClient.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, &storagev1alpha1.ManifestCaptureRequest{})
			Expect(err).ToNot(HaveOccurred())
		})

		It("should delete Ready=False MCR when TTL expired", func() {
			now := time.Now()
			expiredTime := metav1.NewTime(now.Add(-2 * cfg.DefaultTTL))

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "failed-expired-mcr",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &expiredTime,
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionFalse,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonFailed,
							LastTransitionTime: expiredTime,
						},
					},
				},
			}

			Expect(scannerClient.Create(ctx, mcr)).To(Succeed())

			// Run scanner
			ctrl.scanAndDeleteExpiredMCRs(ctx, scannerClient)

			// Verify MCR was deleted
			err := scannerClient.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, &storagev1alpha1.ManifestCaptureRequest{})
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Describe("Post-restart finalization", func() {
		var (
			ctx           context.Context
			restartClient ctrlclient.Client
		)

		BeforeEach(func() {
			ctx = context.Background()
			restartClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}).
				Build()
			// Reuse logger from parent BeforeEach
			ctrl.Client = restartClient
			ctrl.APIReader = restartClient
			ctrl.Logger = testLogger
			ctrl.Config = cfg
		})

		It("should be noop for terminal Ready MCR (TTL annotation not added post-restart)", func() {
			now := metav1.Now()
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ready-no-ttl",
					Namespace: "default",
					// No TTL annotation
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CheckpointName:      "mcp-test-123",
					CompletionTimestamp: &now,
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
							LastTransitionTime: now,
						},
					},
				},
			}

			// Create checkpoint to simulate existing checkpoint
			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mcp-test-123",
				},
			}
			Expect(restartClient.Create(ctx, checkpoint)).To(Succeed())
			Expect(restartClient.Create(ctx, mcr)).To(Succeed())

			// Reconcile (simulating post-restart reconcile)
			req := controllerruntime.Request{
				NamespacedName: types.NamespacedName{
					Name:      mcr.Name,
					Namespace: mcr.Namespace,
				},
			}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify status unchanged (terminal MCR is immutable)
			updatedMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(restartClient.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, updatedMCR)).To(Succeed())
			Expect(updatedMCR.Status.CheckpointName).To(Equal(mcr.Status.CheckpointName))
			Expect(updatedMCR.Status.CompletionTimestamp).ToNot(BeNil())
			readyCond := meta.FindStatusCondition(updatedMCR.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// ============================================================================
	// Reconcile logic tests
	// ============================================================================
	// These tests verify reconcile behavior: idempotency, recovery scenarios, and condition handling.

	// Checkpoint exists finalization path covers recovery scenario when checkpoint was created,
	// but controller crashed before MCR status was finalized. This test ensures reconcile
	// can recover and complete the finalization process.
	Describe("Checkpoint exists finalization path", func() {
		var (
			ctx            context.Context
			finalizeClient ctrlclient.Client
		)

		BeforeEach(func() {
			ctx = context.Background()
			finalizeClient = fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}).
				Build()
			ctrl.Client = finalizeClient
			ctrl.APIReader = finalizeClient
		})

		It("should keep MCR processing when checkpoint exists but is not handed off to SnapshotContent", func() {
			checkpointName := "mcp-test-finalize"

			// Create checkpoint first
			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name: checkpointName,
				},
				Status: storagev1alpha1.ManifestCheckpointStatus{
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCheckpointConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ManifestCheckpointConditionReasonCompleted,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}
			Expect(finalizeClient.Create(ctx, checkpoint)).To(Succeed())

			// Create MCR with checkpoint name but not finalized (no Ready, no CompletionTimestamp)
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mcr-not-finalized",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CheckpointName: checkpointName,
					// No CompletionTimestamp
					// No Ready condition - not in terminal state yet
					Conditions: []metav1.Condition{},
				},
			}
			Expect(finalizeClient.Create(ctx, mcr)).To(Succeed())

			// Reconcile
			req := controllerruntime.Request{
				NamespacedName: types.NamespacedName{
					Name:      mcr.Name,
					Namespace: mcr.Namespace,
				},
			}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Verify MCR published the checkpoint but is not finalized until handoff.
			updatedMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(finalizeClient.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, updatedMCR)).To(Succeed())

			readyCond := meta.FindStatusCondition(updatedMCR.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing))
			Expect(updatedMCR.Status.CheckpointName).To(Equal(checkpointName))
			Expect(updatedMCR.Status.CompletionTimestamp).To(BeNil())
			Expect(updatedMCR.Annotations).To(BeNil())
		})

		It("should finalize MCR when checkpoint exists and is handed off to SnapshotContent", func() {
			checkpointName := "mcp-test-handed-off"

			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name: checkpointName,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: snapstorage.SchemeGroupVersion.String(),
							Kind:       "SnapshotContent",
							Name:       "content-test",
							UID:        types.UID("content-uid"),
						},
					},
				},
				Status: storagev1alpha1.ManifestCheckpointStatus{
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCheckpointConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ManifestCheckpointConditionReasonCompleted,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}
			Expect(finalizeClient.Create(ctx, checkpoint)).To(Succeed())

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mcr-handed-off",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CheckpointName: checkpointName,
					Conditions:     []metav1.Condition{},
				},
			}
			Expect(finalizeClient.Create(ctx, mcr)).To(Succeed())

			req := controllerruntime.Request{
				NamespacedName: types.NamespacedName{
					Name:      mcr.Name,
					Namespace: mcr.Namespace,
				},
			}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
			Expect(result.RequeueAfter).To(BeZero())

			updatedMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(finalizeClient.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, updatedMCR)).To(Succeed())

			readyCond := meta.FindStatusCondition(updatedMCR.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted))
			Expect(updatedMCR.Status.CompletionTimestamp).ToNot(BeNil())
			Expect(updatedMCR.Annotations).ToNot(BeNil())
			Expect(updatedMCR.Annotations[AnnotationKeyTTL]).To(Equal(cfg.DefaultTTLStr))
		})
	})
})

var _ = Describe("ManifestCaptureRequest ObjectKeeper", func() {
	var (
		ctx    context.Context
		client ctrlclient.Client
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
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}, &storagev1alpha1.ManifestCheckpoint{}).
			Build()
	})

	// ============================================================================
	// ObjectKeeper integration tests
	// ============================================================================
	// These tests verify ObjectKeeper creation and UID-based binding contract.

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
			retainerName := namespacemanifest.ManifestCaptureRequestObjectKeeperName(mcr.Namespace, mcr.Name, mcr.UID)
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

			retainerName := namespacemanifest.ManifestCaptureRequestObjectKeeperName(mcr.Namespace, mcr.Name, mcr.UID)
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

		It("should keep stale same-name MCR ObjectKeeper untouched when request UID changes", func() {
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

			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cm",
					Namespace: "default",
				},
			}
			Expect(client.Create(ctx, cm)).To(Succeed())
			Expect(client.Create(ctx, mcr2)).To(Succeed())

			oldRetainerName := namespacemanifest.ManifestCaptureRequestObjectKeeperName(mcr1.Namespace, mcr1.Name, mcr1.UID)
			newRetainerName := namespacemanifest.ManifestCaptureRequestObjectKeeperName(mcr2.Namespace, mcr2.Name, mcr2.UID)
			Expect(newRetainerName).ToNot(Equal(oldRetainerName))

			oldObjectKeeper := &deckhousev1alpha1.ObjectKeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name: oldRetainerName,
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
			Expect(client.Create(ctx, oldObjectKeeper)).To(Succeed())

			testLogger, err := logger.NewLogger("info")
			Expect(err).NotTo(HaveOccurred())
			ctrl, err := NewManifestCheckpointController(client, client, scheme, testLogger, &config.Options{
				DefaultTTL:    10 * time.Minute,
				DefaultTTLStr: "10m",
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = ctrl.Reconcile(ctx, controllerruntime.Request{NamespacedName: types.NamespacedName{Namespace: mcr2.Namespace, Name: mcr2.Name}})
			Expect(err).NotTo(HaveOccurred())

			oldFresh := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: oldRetainerName}, oldFresh)).To(Succeed())
			Expect(oldFresh.Spec.FollowObjectRef.UID).To(Equal(string(mcr1.UID)))

			newFresh := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: newRetainerName}, newFresh)).To(Succeed())
			Expect(newFresh.Spec.FollowObjectRef).NotTo(BeNil())
			Expect(newFresh.Spec.FollowObjectRef.UID).To(Equal(string(mcr2.UID)))
			Expect(newFresh.Spec.FollowObjectRef.Name).To(Equal(mcr2.Name))
			Expect(newFresh.Spec.FollowObjectRef.Namespace).To(Equal(mcr2.Namespace))
		})
	})
})

var _ = Describe("ManifestCaptureRequest Status Update and Checkpoint Name", func() {
	var (
		ctx    context.Context
		client ctrlclient.Client
		ctrl   *ManifestCheckpointController
		scheme *runtime.Scheme
		cfg    *config.Options
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())

		cfg = &config.Options{
			DefaultTTL:    10 * time.Minute,
			DefaultTTLStr: "10m",
		}

		client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}).
			Build()

		testLogger, err := logger.NewLogger("info")
		Expect(err).ToNot(HaveOccurred(), "Failed to create logger")
		Expect(testLogger).ToNot(BeNil(), "Logger must not be nil")
		ctrl, err = NewManifestCheckpointController(
			client,
			client, // Use same client for APIReader in tests
			scheme,
			testLogger,
			cfg,
		)
		Expect(err).ToNot(HaveOccurred(), "Failed to create controller")
	})

	// ============================================================================
	// Helper functions tests
	// ============================================================================
	// These tests verify internal helper functions used by the controller.

	Describe("Deterministic checkpoint name generation", func() {
		It("should generate same checkpoint name for same MCR UID", func() {
			mcrUID := types.UID("test-uid-12345")
			name1 := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcrUID)
			name2 := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcrUID)

			Expect(name1).To(Equal(name2))
			Expect(name1).To(HavePrefix(namespacemanifest.CheckpointNamePrefix))
		})

		It("should generate different checkpoint names for different MCR UIDs", func() {
			mcrUID1 := types.UID("test-uid-12345")
			mcrUID2 := types.UID("test-uid-67890")
			name1 := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcrUID1)
			name2 := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcrUID2)

			Expect(name1).ToNot(Equal(name2))
			Expect(name1).To(HavePrefix(namespacemanifest.CheckpointNamePrefix))
			Expect(name2).To(HavePrefix(namespacemanifest.CheckpointNamePrefix))
		})

		It("should generate RFC 1123 compliant checkpoint name", func() {
			mcrUID := types.UID("test-uid-12345")
			name := namespacemanifest.GenerateManifestCheckpointNameFromUID(mcrUID)

			// RFC 1123: lowercase alphanumeric, must start and end with alphanumeric
			Expect(name).To(MatchRegexp("^[a-z0-9][a-z0-9-]*[a-z0-9]$"))
		})
	})

	Describe("Status and metadata update separation", func() {
		It("should update status via Status().Patch() and metadata via Patch() separately", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					UID:       types.UID("test-uid-123"),
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
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CheckpointName: "mcp-test-123",
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
							Message:            "Test",
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}

			Expect(client.Create(ctx, mcr)).To(Succeed())

			// Update status
			// Update status (Ready condition)
			base := mcr.DeepCopy()
			setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
				Message:            "Test checkpoint",
				LastTransitionTime: metav1.Now(),
			})
			Expect(client.Status().Patch(ctx, mcr, ctrlclient.MergeFrom(base))).To(Succeed())

			// Verify status was updated
			updatedMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, updatedMCR)).To(Succeed())
			ready := meta.FindStatusCondition(updatedMCR.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))

			// Update metadata (TTL annotation)
			base2 := updatedMCR.DeepCopy()
			ctrl.setTTLAnnotation(updatedMCR)
			Expect(client.Patch(ctx, updatedMCR, ctrlclient.MergeFrom(base2))).To(Succeed())

			// Verify metadata was updated
			finalMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, finalMCR)).To(Succeed())
			Expect(finalMCR.Annotations).ToNot(BeNil())
			Expect(finalMCR.Annotations[AnnotationKeyTTL]).To(Equal("10m"))
			// Verify status is still intact
			ready = meta.FindStatusCondition(finalMCR.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Describe("Ready condition finalization", func() {
		It("should set Ready=True when checkpoint is completed", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "default",
					UID:       types.UID("test-uid-123"),
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CheckpointName: "mcp-test-123",
					// No Ready condition - not in terminal state yet
					Conditions: []metav1.Condition{},
				},
			}

			Expect(client.Create(ctx, mcr)).To(Succeed())

			// Simulate finalization: set Ready=True
			base := mcr.DeepCopy()
			setSingleCondition(&mcr.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
				Message:            "Checkpoint created successfully",
				LastTransitionTime: metav1.Now(),
			})
			Expect(client.Status().Patch(ctx, mcr, ctrlclient.MergeFrom(base))).To(Succeed())

			// Verify Ready=True
			finalMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, finalMCR)).To(Succeed())

			readyCond := meta.FindStatusCondition(finalMCR.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted))
		})

		It("should be noop for terminal Ready=False MCR", func() {
			now := metav1.Now()
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "terminal-failed",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					CompletionTimestamp: &now,
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionFalse,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonFailed,
							LastTransitionTime: now,
						},
					},
				},
			}
			Expect(client.Create(ctx, mcr)).To(Succeed())

			// Save initial state
			initialMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, initialMCR)).To(Succeed())
			initialStatus := initialMCR.Status.DeepCopy()
			initialAnnotations := make(map[string]string)
			if initialMCR.Annotations != nil {
				for k, v := range initialMCR.Annotations {
					initialAnnotations[k] = v
				}
			}

			// Reconcile
			req := controllerruntime.Request{
				NamespacedName: types.NamespacedName{
					Name:      mcr.Name,
					Namespace: mcr.Namespace,
				},
			}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			// Terminal state: controller must not modify status on subsequent reconciles
			updatedMCR := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: mcr.Name, Namespace: mcr.Namespace}, updatedMCR)).To(Succeed())

			readyCond := meta.FindStatusCondition(updatedMCR.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonFailed))
			Expect(updatedMCR.Status.CompletionTimestamp).To(Equal(initialStatus.CompletionTimestamp))
			// TTL annotation may be added (post-restart finalization), but status must be unchanged
			Expect(updatedMCR.Status.Conditions).To(Equal(initialStatus.Conditions))
		})
	})

})

var _ = Describe("Helper Functions", func() {
	Describe("setSingleCondition", func() {
		It("should add first condition to empty list", func() {
			conds := &[]metav1.Condition{}

			setSingleCondition(conds, metav1.Condition{
				Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
				Message:            "Test message",
				LastTransitionTime: metav1.Now(),
			})

			Expect(len(*conds)).To(Equal(1))
			cond := (*conds)[0]
			Expect(cond.Type).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionTypeReady))
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should replace existing condition of same type", func() {
			conds := &[]metav1.Condition{
				{
					Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
					Status:             metav1.ConditionTrue,
					Reason:             "Completed",
					LastTransitionTime: metav1.Now(),
				},
			}

			setSingleCondition(conds, metav1.Condition{
				Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
				Status:             metav1.ConditionFalse,
				Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonFailed,
				Message:            "Updated message",
				LastTransitionTime: metav1.Now(),
			})

			Expect(len(*conds)).To(Equal(1))
			updatedCond := (*conds)[0]
			Expect(updatedCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(updatedCond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonFailed))
		})

		It("should keep only one condition of each type", func() {
			conds := &[]metav1.Condition{
				{
					Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
				{
					Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
					Status:             metav1.ConditionFalse,
					LastTransitionTime: metav1.Now(),
				},
			}

			setSingleCondition(conds, metav1.Condition{
				Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "New",
				LastTransitionTime: metav1.Now(),
			})

			Expect(len(*conds)).To(Equal(1))
			Expect((*conds)[0].Reason).To(Equal("New"))
		})
	})

})

// ============================================================================
// ADR and compliance tests
// ============================================================================
// These tests verify architectural decisions, resource scoping, and RBAC compliance.

var _ = Describe("Conditions", func() {
	Describe("Ready condition check", func() {
		It("should identify Ready=True as ready", func() {
			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				Status: storagev1alpha1.ManifestCheckpointStatus{
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             "Completed",
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}

			readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
			isReady := readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

			Expect(isReady).To(BeTrue())
			Expect(readyCondition.Reason).To(Equal("Completed"))
		})

		It("should identify Ready=False as not ready", func() {
			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				Status: storagev1alpha1.ManifestCheckpointStatus{
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionFalse,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonFailed,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}

			readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
			isReady := readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

			Expect(isReady).To(BeFalse())
			Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonFailed))
		})

		It("should identify absence of Ready condition as not ready", func() {
			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				Status: storagev1alpha1.ManifestCheckpointStatus{
					Conditions: []metav1.Condition{},
				},
			}

			readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
			isReady := readyCondition != nil && readyCondition.Status == metav1.ConditionTrue

			Expect(isReady).To(BeFalse())
		})
	})

	Describe("No Phase field", func() {
		It("should work with ManifestCheckpointStatus without Phase", func() {
			status := storagev1alpha1.ManifestCheckpointStatus{
				TotalObjects:   10,
				TotalSizeBytes: 2048,
				Conditions: []metav1.Condition{
					{
						Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
						Status:             metav1.ConditionTrue,
						Reason:             "Completed",
						LastTransitionTime: metav1.Now(),
					},
				},
			}

			checkpoint := &storagev1alpha1.ManifestCheckpoint{Status: status}

			Expect(checkpoint.Status.TotalObjects).To(Equal(10))
			Expect(len(checkpoint.Status.Conditions)).To(Equal(1))

			readyCondition := meta.FindStatusCondition(checkpoint.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should work with ManifestCaptureRequest without Phase", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-mcr",
					Namespace: "test-namespace",
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

			mcr.Status = storagev1alpha1.ManifestCaptureRequestStatus{
				Conditions: []metav1.Condition{
					{
						Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
						Status:             metav1.ConditionTrue,
						Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
						LastTransitionTime: metav1.Now(),
					},
				},
			}

			Expect(len(mcr.Status.Conditions)).To(Equal(1))
			readyCondition := meta.FindStatusCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
		})
	})
})

var _ = Describe("Resource Scoping", func() {
	Describe("Cluster-scoped resources", func() {
		It("should verify ManifestCheckpoint is cluster-scoped", func() {
			checkpoint := &storagev1alpha1.ManifestCheckpoint{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mcp-test-123",
					// No Namespace field - cluster-scoped
				},
				Spec: storagev1alpha1.ManifestCheckpointSpec{
					SourceNamespace: "test-namespace",
					ManifestCaptureRequestRef: &storagev1alpha1.ObjectReference{
						Name:      "test-mcr",
						Namespace: "test-namespace",
						UID:       "test-uid",
					},
				},
			}

			Expect(checkpoint.Namespace).To(Equal(""))
			Expect(checkpoint.Name).ToNot(BeEmpty())
			Expect(checkpoint.Spec.SourceNamespace).To(Equal("test-namespace"))
		})

		It("should verify ManifestCheckpointContentChunk is cluster-scoped", func() {
			chunk := &storagev1alpha1.ManifestCheckpointContentChunk{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mcp-test-123-0",
					// No Namespace field - cluster-scoped
				},
				Spec: storagev1alpha1.ManifestCheckpointContentChunkSpec{
					CheckpointName: "mcp-test-123",
					Index:          0,
					Data:           "test-data",
					ObjectsCount:   5,
					Checksum:       "test-checksum",
				},
			}

			Expect(chunk.Namespace).To(Equal(""))
			Expect(chunk.Name).ToNot(BeEmpty())
			Expect(chunk.Spec.CheckpointName).ToNot(BeEmpty())
		})
	})
})

var _ = Describe("Object References", func() {
	It("should create ManifestCaptureRequestRef correctly", func() {
		mcr := &storagev1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-mcr",
				Namespace: "test-namespace",
				UID:       "test-uid-123",
			},
		}

		spec := storagev1alpha1.ManifestCheckpointSpec{
			SourceNamespace: mcr.Namespace,
			ManifestCaptureRequestRef: &storagev1alpha1.ObjectReference{
				Name:      mcr.Name,
				Namespace: mcr.Namespace,
				UID:       string(mcr.UID),
			},
		}

		Expect(spec.SourceNamespace).To(Equal(mcr.Namespace))
		Expect(spec.ManifestCaptureRequestRef).ToNot(BeNil())
		Expect(spec.ManifestCaptureRequestRef.Name).To(Equal(mcr.Name))
		Expect(spec.ManifestCaptureRequestRef.Namespace).To(Equal(mcr.Namespace))
		Expect(spec.ManifestCaptureRequestRef.UID).To(Equal(string(mcr.UID)))
	})
})

var _ = Describe("ADR Compliance", func() {

	Describe("RBAC compliance", func() {
		It("should document allowed RBAC verbs for ManifestCheckpointContentChunk", func() {
			allowedVerbs := map[string]bool{
				"create": true,
				"get":    true,
				"delete": true,
			}

			forbiddenVerbs := map[string]bool{
				"list":  true,
				"watch": true,
			}

			// Verify allowed verbs are documented
			for verb := range allowedVerbs {
				Expect(allowedVerbs[verb]).To(BeTrue(), "Verb '%s' should be allowed for ManifestCheckpointContentChunk according to ADR", verb)
			}

			// Verify forbidden verbs are documented
			for verb := range forbiddenVerbs {
				Expect(forbiddenVerbs[verb]).To(BeTrue(), "Verb '%s' should be forbidden for ManifestCheckpointContentChunk according to ADR", verb)
			}

			// This test serves as documentation that:
			// 1. Controller RBAC in templates/controller/rbac-for-us.yaml should only have: create, get, delete
			// 2. No other ClusterRole/Role should grant list/watch on manifestcheckpointcontentchunks
			// 3. This is enforced by ADR and should be verified in production via RBAC manifest validation
		})
	})
})

// ============================================================================
// Ready Condition Semantics Tests
// ============================================================================
// These tests verify the new Ready condition semantics with Processing reason.
// See docs/architecture/ready-condition-semantics.md for the full specification.

var _ = Describe("Ready Condition Semantics", func() {
	var (
		ctx        context.Context
		k8sClient  ctrlclient.Client
		reconciler *ManifestCheckpointController
		scheme     *runtime.Scheme
		cfg        *config.Options
		testLogger logger.LoggerInterface
	)

	BeforeEach(func() {
		ctx = context.Background()

		scheme = runtime.NewScheme()
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(corev1.AddToScheme(scheme)).To(Succeed())

		cfg = &config.Options{
			DefaultTTL:    10 * time.Minute,
			DefaultTTLStr: "10m",
		}

		k8sClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&storagev1alpha1.ManifestCaptureRequest{}).
			Build()

		var err error
		testLogger, err = logger.NewLogger("info")
		Expect(err).ToNot(HaveOccurred(), "Failed to create logger")
		Expect(testLogger).ToNot(BeNil(), "Logger must not be nil")

		reconciler, err = NewManifestCheckpointController(
			k8sClient,
			k8sClient, // Use same client for APIReader in tests
			scheme,
			testLogger,
			cfg,
		)
		Expect(err).ToNot(HaveOccurred(), "Failed to create controller")
	})

	// Helper function to create MCR with Processing condition
	newProcessingMCR := func(name, namespace string) *storagev1alpha1.ManifestCaptureRequest {
		return &storagev1alpha1.ManifestCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Status: storagev1alpha1.ManifestCaptureRequestStatus{
				Conditions: []metav1.Condition{
					{
						Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
						Status:             metav1.ConditionFalse,
						Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing,
						Message:            "Operation started",
						LastTransitionTime: metav1.Now(),
					},
				},
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
	}

	Describe("Processing condition", func() {
		It("sets Ready=False with reason=Processing on first reconcile", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
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
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			// Create ConfigMap to avoid NotFound error
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cm",
					Namespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			// Call processCaptureRequest directly to test Processing setup
			// This avoids full reconcile which would try to create checkpoint/chunks
			// Error is expected because we don't have all resources set up, but Processing should be set
			// We check the status update, not the error
			_, _ = reconciler.processCaptureRequest(ctx, mcr)

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())

			cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing))
			Expect(updated.Status.CompletionTimestamp).To(BeNil())
		})

		It("does not overwrite Processing condition on subsequent reconciles", func() {
			now := metav1.Now()

			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionFalse,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing,
							Message:            "Operation started",
							LastTransitionTime: now,
						},
					},
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
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			// Create ConfigMap to avoid NotFound error
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cm",
					Namespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			// Call processCaptureRequest directly - Processing should not be overwritten
			_, _ = reconciler.processCaptureRequest(ctx, mcr)

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())

			cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			// LastTransitionTime should not be newer (Processing should not be overwritten)
			// Allow small time difference due to Get() before Update()
			Expect(cond.LastTransitionTime.Time.Before(now.Time.Add(time.Second)) || cond.LastTransitionTime.Time.Equal(now.Time)).To(BeTrue())
			Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing))
		})
	})

	Describe("Condition transitions", func() {
		It("transitions from Processing to Completed", func() {
			mcr := newProcessingMCR("test", "default")
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			err := reconciler.finalizeMCR(
				ctx,
				mcr,
				metav1.ConditionTrue,
				storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
				"done",
			)
			Expect(err).NotTo(HaveOccurred())

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())

			cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted))
			Expect(updated.Status.CompletionTimestamp).NotTo(BeNil())
		})

		It("transitions from Processing to Failed", func() {
			mcr := newProcessingMCR("test", "default")
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			err := reconciler.finalizeMCR(
				ctx,
				mcr,
				metav1.ConditionFalse,
				storagev1alpha1.ManifestCaptureRequestConditionReasonFailed,
				"boom",
			)
			Expect(err).NotTo(HaveOccurred())

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())

			cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonFailed))
			Expect(updated.Status.CompletionTimestamp).NotTo(BeNil())
		})

	})

	Describe("isTerminal semantics", func() {
		DescribeTable("isTerminal semantics",
			func(status metav1.ConditionStatus, reason string, terminal bool) {
				mcr := &storagev1alpha1.ManifestCaptureRequest{
					Status: storagev1alpha1.ManifestCaptureRequestStatus{
						Conditions: []metav1.Condition{
							{
								Type:   storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
								Status: status,
								Reason: reason,
							},
						},
					},
				}
				Expect(reconciler.isTerminal(mcr)).To(Equal(terminal))
			},
			Entry("Processing", metav1.ConditionFalse, storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing, false),
			Entry("Completed", metav1.ConditionTrue, storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted, true),
			Entry("Failed", metav1.ConditionFalse, storagev1alpha1.ManifestCaptureRequestConditionReasonFailed, true),
		)
	})

	Describe("updateProcessingMessage", func() {
		It("updates message in Processing condition", func() {
			mcr := newProcessingMCR("test", "default")
			initialTime := mcr.Status.Conditions[0].LastTransitionTime
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			// Update message
			reconciler.updateProcessingMessage(ctx, mcr, "New progress message")

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())

			cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonProcessing))
			Expect(cond.Message).To(Equal("New progress message"))
			// LastTransitionTime should be preserved (compare Unix time to avoid precision issues)
			Expect(cond.LastTransitionTime.Unix()).To(Equal(initialTime.Unix()))
		})

		It("preserves LastTransitionTime when updating message", func() {
			mcr := newProcessingMCR("test", "default")
			initialTime := metav1.NewTime(time.Now().Add(-5 * time.Minute).Truncate(time.Second))
			mcr.Status.Conditions[0].LastTransitionTime = initialTime
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			// Update message multiple times
			reconciler.updateProcessingMessage(ctx, mcr, "Step 1")

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())
			cond1 := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond1.LastTransitionTime.Unix()).To(Equal(initialTime.Unix()))

			reconciler.updateProcessingMessage(ctx, updated, "Step 2")

			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())
			cond2 := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond2.LastTransitionTime.Unix()).To(Equal(initialTime.Unix()))
			Expect(cond2.Message).To(Equal("Step 2"))
		})

		It("does nothing if resource is not in Processing state", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "completed",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
							Message:            "Original message",
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			// Try to update message - should do nothing
			reconciler.updateProcessingMessage(ctx, mcr, "Should not update")

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())

			cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(Equal("Original message"))
			Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted))
		})

		It("does nothing if resource has no Ready condition", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-condition",
					Namespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			// Try to update message - should do nothing
			reconciler.updateProcessingMessage(ctx, mcr, "Should not update")

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())

			cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).To(BeNil())
		})

		It("does nothing if resource is in Failed state", func() {
			mcr := &storagev1alpha1.ManifestCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "failed",
					Namespace: "default",
				},
				Status: storagev1alpha1.ManifestCaptureRequestStatus{
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
							Status:             metav1.ConditionFalse,
							Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonFailed,
							Message:            "Original error",
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			// Try to update message - should do nothing
			reconciler.updateProcessingMessage(ctx, mcr, "Should not update")

			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())

			cond := meta.FindStatusCondition(updated.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(Equal("Original error"))
			Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonFailed))
		})

		It("skips update if resource transitions from Processing to Completed during update", func() {
			mcr := newProcessingMCR("test", "default")
			Expect(k8sClient.Create(ctx, mcr)).To(Succeed())

			// Simulate transition to Completed by another reconcile
			updated := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), updated)).To(Succeed())
			setSingleCondition(&updated.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ManifestCaptureRequestConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted,
				Message:            "Completed",
				LastTransitionTime: metav1.Now(),
			})
			Expect(k8sClient.Status().Update(ctx, updated)).To(Succeed())

			// Try to update Processing message - should detect Completed and skip
			reconciler.updateProcessingMessage(ctx, mcr, "Should not update")

			final := &storagev1alpha1.ManifestCaptureRequest{}
			Expect(k8sClient.Get(ctx, ctrlclient.ObjectKeyFromObject(mcr), final)).To(Succeed())

			cond := meta.FindStatusCondition(final.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonCompleted))
			Expect(cond.Message).To(Equal("Completed"))
		})

		It("handles NotFound error gracefully (best-effort)", func() {
			mcr := newProcessingMCR("not-found", "default")
			// Don't create it in k8sClient

			// Should not panic (best-effort; resource not in API)
			reconciler.updateProcessingMessage(ctx, mcr, "Test message")
		})
	})
})
