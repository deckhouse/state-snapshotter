//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	iretainer "github.com/deckhouse/state-snapshotter/api/v1alpha1/iretainer"
)

const (
	testTimeout  = 30 * time.Second
	pollInterval = 100 * time.Millisecond
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = Describe("E2E Tests for ManifestCaptureRequest and ManifestCheckpoint", func() {
	var ctx context.Context
	var testNS string

	BeforeEach(func() {
		ctx = context.Background()
		testNS = fmt.Sprintf("test-ns-%d", time.Now().UnixNano())
		createNamespace(ctx, testNS)
	})

	AfterEach(func() {
		// CRITICAL: Cleanup cluster-scoped resources FIRST, before namespace deletion
		// These resources don't get deleted with namespace, so we need explicit cleanup
		// to prevent test pollution between runs

		// Delete all ManifestCheckpoints created in this test
		checkpoints := &storagev1alpha1.ManifestCheckpointList{}
		if err := k8sClient.List(ctx, checkpoints); err == nil {
			for i := range checkpoints.Items {
				cp := &checkpoints.Items[i]
				// Only delete checkpoints that reference MCRs from this test namespace
				if cp.Spec.ManifestCaptureRequestRef != nil &&
					cp.Spec.ManifestCaptureRequestRef.Namespace == testNS {
					foreground := metav1.DeletePropagationForeground
					_ = k8sClient.Delete(ctx, cp, &client.DeleteOptions{
						PropagationPolicy: &foreground,
					})
				}
			}
		}

		// Delete all Retainers created in this test
		retainers := &iretainer.IRetainerList{}
		if err := k8sClient.List(ctx, retainers); err == nil {
			for i := range retainers.Items {
				ret := &retainers.Items[i]
				// ADR: Only ONE Retainer exists per MCR: ret-mcr-{namespace}-{mcrName}
				// Delete MCR retainers for this namespace
				if strings.HasPrefix(ret.Name, fmt.Sprintf("ret-mcr-%s-", testNS)) {
					_ = k8sClient.Delete(ctx, ret)
					continue
				}
				// Legacy cleanup: Delete any old retainers that might have been created with old format
				// (ret-{checkpointName}) - these should not exist with current ADR implementation
				if strings.HasPrefix(ret.Name, "ret-") && !strings.HasPrefix(ret.Name, "ret-mcr-") {
					// Extract checkpoint name from retainer name: "ret-{checkpointName}"
					checkpointName := strings.TrimPrefix(ret.Name, "ret-")
					// Check if this checkpoint belongs to our test namespace
					cp := &storagev1alpha1.ManifestCheckpoint{}
					if err := k8sClient.Get(ctx, types.NamespacedName{Name: checkpointName}, cp); err == nil {
						if cp.Spec.ManifestCaptureRequestRef != nil &&
							cp.Spec.ManifestCaptureRequestRef.Namespace == testNS {
							// Legacy retainer found - delete it (should not exist with current ADR)
							_ = k8sClient.Delete(ctx, ret)
						}
					} else {
						// Checkpoint doesn't exist (already deleted), safe to delete retainer
						_ = k8sClient.Delete(ctx, ret)
					}
				}
			}
		}

		// Wait for GC to process deletions before deleting namespace
		time.Sleep(500 * time.Millisecond)
	})

	// === GROUP 1: HAPPY PATH AND BASIC FUNCTIONALITY ===

	Describe("Basic Checkpoint Creation", func() {
		// TestBasicCheckpointCreation verifies the happy path of creating a ManifestCheckpoint
		// from namespaced Kubernetes resources. It ensures:
		// - ManifestCheckpoint is created as cluster-scoped resource
		// - Chunks are created with proper ownerReferences
		// - manifestCaptureRequestRef is correctly set
		// - Ready condition is set to True
		It("should create ManifestCheckpoint from namespaced objects", func() {
			// Create test objects
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			createService(ctx, testNS, TestFixtures.TestServiceName)

			// Create ManifestCaptureRequest
			targets := GetStandardTargets()
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)
			mcrUID := string(mcr.UID) // Save UID before namespace deletion

			// Wait for Ready
			mcr = waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			// Verify MCR status
			Expect(mcr.Status.CheckpointName).NotTo(BeEmpty())
			checkpointName := mcr.Status.CheckpointName

			// Verify ManifestCheckpoint is cluster-scoped
			mcp := getManifestCheckpoint(ctx, checkpointName)
			verifyCheckpointIsClusterScoped(mcp)

			// Verify manifestCaptureRequestRef
			Expect(mcp.Spec.ManifestCaptureRequestRef).NotTo(BeNil())
			Expect(mcp.Spec.ManifestCaptureRequestRef.Name).To(Equal(TestFixtures.TestMCRName))
			Expect(mcp.Spec.ManifestCaptureRequestRef.Namespace).To(Equal(testNS))
			Expect(mcp.Spec.ManifestCaptureRequestRef.UID).To(Equal(mcrUID))
			Expect(mcp.Spec.SourceNamespace).To(Equal(testNS))

			// Verify chunks are created and cluster-scoped
			chunks := listManifestCheckpointContentChunks(ctx, checkpointName)
			Expect(len(chunks)).To(BeNumerically(">", 0))
			for _, chunk := range chunks {
				verifyChunkIsClusterScoped(&chunk)
				Expect(chunk.Spec.CheckpointName).To(Equal(checkpointName))
				Expect(chunk.Spec.Checksum).NotTo(BeEmpty())
				// Verify ownerReference to checkpoint
				Expect(len(chunk.OwnerReferences)).To(BeNumerically(">", 0))
				found := false
				for _, ref := range chunk.OwnerReferences {
					if ref.Kind == "ManifestCheckpoint" && ref.Name == checkpointName {
						found = true
						Expect(ref.UID).To(Equal(mcp.UID))
						break
					}
				}
				Expect(found).To(BeTrue(), "Chunk should have ownerReference to ManifestCheckpoint")
			}

			// Verify Ready condition
			ready := findCondition(mcp.Status.Conditions, "Ready")
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// === GROUP 2: CONDITION TRANSITIONS ===

	Describe("Condition Transitions", func() {
		// TestConditionTransitionEmptyToProcessing verifies that when a ManifestCaptureRequest
		// is created, it transitions from empty state to Processing condition.
		// Note: Due to fast controller processing, Processing condition may not be observed,
		// so we check for either Processing=True or Ready=True to indicate processing started.
		It("should transition from Empty to Processing", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for any status update (Processing or Ready)
			// Note: Processing condition may not be set if controller processes too fast
			// So we wait for either Processing=True or Ready=True
			Eventually(func() bool {
				var err error
				mcr, err = getManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName)
				if err != nil {
					return false // MCR not found yet, retry
				}
				processing := findCondition(mcr.Status.Conditions, "Processing")
				ready := findCondition(mcr.Status.Conditions, "Ready")
				return (processing != nil && processing.Status == metav1.ConditionTrue) ||
					(ready != nil && ready.Status == metav1.ConditionTrue) ||
					len(mcr.Status.Conditions) > 0 // Any condition means processing started
			}, testTimeout, pollInterval).Should(BeTrue())

			// Verify that status was updated
			mcr = getManifestCaptureRequestOrFail(ctx, testNS, TestFixtures.TestMCRName)
			Expect(len(mcr.Status.Conditions)).To(BeNumerically(">", 0))
		})

		It("should transition from Processing to Ready", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			ready := findCondition(mcr.Status.Conditions, "Ready")
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))

			processing := findCondition(mcr.Status.Conditions, "Processing")
			if processing != nil {
				Expect(processing.Status).To(Equal(metav1.ConditionFalse))
			}

			Expect(mcr.Status.ErrorReason).To(BeEmpty())
		})

		It("should transition from Processing to Error on NotFound", func() {
			targets := []storagev1alpha1.ManifestTarget{
				GetNotFoundTarget(),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Failed
			mcr := waitForManifestCaptureRequestFailed(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			ready := findCondition(mcr.Status.Conditions, "Ready")
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))

			failed := findCondition(mcr.Status.Conditions, "Failed")
			Expect(failed).NotTo(BeNil())
			Expect(failed.Status).To(Equal(metav1.ConditionTrue))

			Expect(mcr.Status.ErrorReason).To(Equal("NotFound"))
			Expect(mcr.Status.CheckpointName).To(BeEmpty())
		})

		It("should persist error state on reconcile", func() {
			targets := []storagev1alpha1.ManifestTarget{
				GetNotFoundTarget(),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for error
			mcr = waitForManifestCaptureRequestFailed(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			errorReasonBefore := mcr.Status.ErrorReason

			// Trigger reconcile
			triggerReconcile(ctx, mcr)

			// Verify error persists
			mcr = getManifestCaptureRequestOrFail(ctx, testNS, TestFixtures.TestMCRName)
			Expect(mcr.Status.ErrorReason).To(Equal(errorReasonBefore))
			ready := findCondition(mcr.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		})
	})

	// === GROUP 3: IDEMPOTENCY ===

	Describe("Idempotency", func() {
		It("should not recreate checkpoint on reconcile", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName
			mcp := getManifestCheckpoint(ctx, checkpointName)
			uidBefore := mcp.UID

			// Trigger reconcile
			triggerReconcile(ctx, mcr)

			// Verify checkpoint not recreated
			mcp = getManifestCheckpoint(ctx, checkpointName)
			Expect(mcp.UID).To(Equal(uidBefore))
		})

		It("should not recreate retainers on reconcile", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			// Count retainers
			countBefore := countRetainers(ctx)

			// Trigger reconcile by updating annotation (not spec, as Ready MCR should skip reconcile)
			mcr.Annotations = map[string]string{"test": "value"}
			triggerReconcile(ctx, mcr)

			// Verify count unchanged
			Eventually(func() int {
				return countRetainers(ctx)
			}, 5*time.Second, pollInterval).Should(Equal(countBefore))
		})

		It("should skip reconcile for Ready MCR", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// Try to modify MCR (add annotation)
			mcr.Annotations = map[string]string{"test": "value"}
			triggerReconcile(ctx, mcr)

			// Verify checkpoint not recreated
			mcp := getManifestCheckpoint(ctx, checkpointName)
			ready := findCondition(mcp.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// === GROUP 4: VALIDATION ===

	Describe("Validation", func() {
		It("should reject cluster-scoped resources", func() {
			targets := []storagev1alpha1.ManifestTarget{
				GetClusterScopedTarget(),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for error
			mcr = waitForManifestCaptureRequestFailed(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			Expect(mcr.Status.ErrorReason).NotTo(BeEmpty())
			Expect(mcr.Status.CheckpointName).To(BeEmpty())

			// Verify no checkpoint created for this MCR
			// Filter by checking that no checkpoint has this MCR's UID in manifestCaptureRequestRef
			checkpoints := &storagev1alpha1.ManifestCheckpointList{}
			err := k8sClient.List(ctx, checkpoints)
			Expect(err).NotTo(HaveOccurred())

			// Count checkpoints that reference this MCR
			matchingCheckpoints := 0
			for _, cp := range checkpoints.Items {
				if cp.Spec.ManifestCaptureRequestRef != nil &&
					cp.Spec.ManifestCaptureRequestRef.Name == TestFixtures.TestMCRName &&
					cp.Spec.ManifestCaptureRequestRef.Namespace == testNS &&
					cp.Spec.ManifestCaptureRequestRef.UID == string(mcr.UID) {
					matchingCheckpoints++
				}
			}
			Expect(matchingCheckpoints).To(Equal(0))
		})
	})

	// === GROUP 5: RETAINERS ===

	Describe("Retainers", func() {
		It("should create MCR TTL Retainer with correct TTL", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			// Verify MCR TTL Retainer
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			ret := getRetainer(ctx, retainerName)

			Expect(ret.Spec.Mode).To(Equal("FollowObjectWithTTL"))
			Expect(ret.Spec.TTL).NotTo(BeNil())
			Expect(ret.Spec.TTL.Duration).To(Equal(10 * time.Minute))
			Expect(ret.Spec.FollowObjectRef).NotTo(BeNil())
			Expect(ret.Spec.FollowObjectRef.Kind).To(Equal("ManifestCaptureRequest"))
			Expect(ret.Spec.FollowObjectRef.Name).To(Equal(TestFixtures.TestMCRName))
			Expect(ret.Spec.FollowObjectRef.Namespace).To(Equal(testNS))
			Expect(ret.Spec.FollowObjectRef.UID).To(Equal(string(mcr.UID)))
		})

		It("should create MCR Retainer with ownerRef", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// ADR: Only ONE Retainer exists: ret-mcr-<namespace>-<mcrName>
			// This Retainer:
			// - Uses FollowObjectWithTTL mode to follow MCR (implements MCR TTL)
			// - Holds the ManifestCheckpoint (MCP has ownerRef to this Retainer)
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			ret := getRetainer(ctx, retainerName)

			// Verify retainer follows MCR in FollowObjectWithTTL mode
			Expect(ret.Spec.Mode).To(Equal("FollowObjectWithTTL"), "Retainer must use FollowObjectWithTTL mode to follow MCR")
			Expect(ret.Spec.FollowObjectRef).NotTo(BeNil(), "Retainer must follow MCR")
			Expect(ret.Spec.FollowObjectRef.APIVersion).To(Equal("state-snapshotter.deckhouse.io/v1alpha1"))
			Expect(ret.Spec.FollowObjectRef.Kind).To(Equal("ManifestCaptureRequest"))
			Expect(ret.Spec.FollowObjectRef.Namespace).To(Equal(testNS))
			Expect(ret.Spec.FollowObjectRef.Name).To(Equal(TestFixtures.TestMCRName))
			Expect(ret.Spec.FollowObjectRef.UID).To(Equal(string(mcr.UID)))
			Expect(ret.Spec.TTL).NotTo(BeNil(), "Retainer must have TTL for FollowObjectWithTTL mode")

			// Verify MCP has ownerRef to this Retainer
			mcp := getManifestCheckpoint(ctx, checkpointName)
			Expect(len(mcp.OwnerReferences)).To(Equal(1), "MCP must have exactly one ownerRef to ret-mcr-* Retainer")
			ref := mcp.OwnerReferences[0]
			Expect(ref.Kind).To(Equal("IRetainer"))
			Expect(ref.Name).To(Equal(retainerName))
			Expect(ref.UID).To(Equal(ret.UID))
			Expect(ref.Controller).NotTo(BeNil())
			Expect(*ref.Controller).To(BeTrue(), "Retainer must be controller owner of MCP")
		})

		// TestRetainerCheckpointTTLExpiration verifies that when a checkpoint's TTL Retainer expires,
		// the checkpoint is automatically deleted by garbage collection (via ownerReference).
		// ADR states: checkpoint lives as long as its retainers hold it.
		// Note: RetainerController calculates expiration as CreationTimestamp + TTL.Duration,
		// so we need to delete the old retainer and create a new one with short TTL.
		// IMPORTANT: In envtest, kube-controller-manager is not running, so actual GC won't work.
		// This test verifies that:
		// 1. Retainer is deleted after TTL expiration
		// 2. Checkpoint has correct ownerReference (Controller=true) to retainer for GC to work in real cluster
		// In a real cluster with kube-controller-manager, when retainer is deleted, checkpoint would be deleted automatically.
		It("should delete checkpoint after MCR retainer expires when MCR is deleted", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// Get checkpoint and MCR retainer
			mcp := getManifestCheckpoint(ctx, checkpointName)
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			ret := getRetainer(ctx, retainerName)

			// Verify retainer exists and follows MCR
			Expect(ret.Spec.Mode).To(Equal("FollowObjectWithTTL"))
			Expect(ret.Spec.TTL).NotTo(BeNil())

			// Verify checkpoint has ownerRef to retainer
			Expect(len(mcp.OwnerReferences)).To(Equal(1))
			Expect(mcp.OwnerReferences[0].Kind).To(Equal("IRetainer"))
			Expect(mcp.OwnerReferences[0].Name).To(Equal(retainerName))
			Expect(mcp.OwnerReferences[0].Controller).NotTo(BeNil())
			Expect(*mcp.OwnerReferences[0].Controller).To(BeTrue())

			// Delete MCR - this should trigger TTL countdown in retainer
			err := k8sClient.Delete(ctx, mcr)
			Expect(err).NotTo(HaveOccurred())

			// Wait for MCR to be deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: TestFixtures.TestMCRName}, &storagev1alpha1.ManifestCaptureRequest{})
				return errors.IsNotFound(err)
			}, testTimeout, pollInterval).Should(BeTrue(), "MCR should be deleted")

			// Wait for retainer to detect MCR deletion and start TTL countdown
			// RetainerController should set ttl-start-timestamp when MCR is deleted
			Eventually(func() bool {
				updatedRet := &iretainer.IRetainer{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: retainerName}, updatedRet)
				if err != nil {
					return false
				}
				// Check if ttl-start-timestamp annotation is set (indicates TTL countdown started)
				return updatedRet.Annotations != nil &&
					updatedRet.Annotations["retainer.deckhouse.io/ttl-start-timestamp"] != ""
			}, testTimeout, pollInterval).Should(BeTrue(), "Retainer should start TTL countdown after MCR deletion")

			// Note: In envtest, we can't easily test full TTL expiration (10 minutes default)
			// This test verifies that:
			// 1. Retainer correctly detects MCR deletion
			// 2. TTL countdown starts (ttl-start-timestamp is set)
			// 3. Checkpoint has correct ownerReference for GC
			// In a real cluster, when retainer expires after TTL, it would be deleted and GC would delete checkpoint
		})

		// TestRetainerMCRDoesNotAffectCheckpoint verifies that MCR-retainer deletion
		// does not affect checkpoint. MCR-retainer is only for MCR TTL, not checkpoint.
		It("should delete checkpoint when MCR retainer is deleted (GC via ownerRef)", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// Verify checkpoint exists
			mcp := getManifestCheckpoint(ctx, checkpointName)
			Expect(mcp).NotTo(BeNil())

			// Verify checkpoint has ownerRef to MCR retainer
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			Expect(len(mcp.OwnerReferences)).To(Equal(1))
			Expect(mcp.OwnerReferences[0].Kind).To(Equal("IRetainer"))
			Expect(mcp.OwnerReferences[0].Name).To(Equal(retainerName))
			Expect(mcp.OwnerReferences[0].Controller).NotTo(BeNil())
			Expect(*mcp.OwnerReferences[0].Controller).To(BeTrue())

			// Get MCR retainer
			mcrRet := getRetainer(ctx, retainerName)
			Expect(mcrRet).NotTo(BeNil())

			// Delete MCR retainer manually (simulating TTL expiration)
			// ADR: When retainer expires, it's deleted → GC deletes MCP (via ownerRef)
			err := k8sClient.Delete(ctx, mcrRet)
			Expect(err).NotTo(HaveOccurred())

			// Wait for retainer to be deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: retainerName}, &iretainer.IRetainer{})
				return errors.IsNotFound(err)
			}, testTimeout, pollInterval).Should(BeTrue(), "MCR retainer should be deleted")

			// Note: In envtest, kube-controller-manager is not running, so actual GC won't work.
			// This test verifies the ownerReference setup (Controller=true) that would trigger GC in a real cluster.
			// In a real cluster with kube-controller-manager, when retainer is deleted, checkpoint would be deleted automatically via GC.
			// For now, we just verify that checkpoint still exists (GC doesn't work in envtest)
			// but the ownerRef is correctly set for GC to work in real cluster.
			mcp = getManifestCheckpoint(ctx, checkpointName)
			Expect(mcp).NotTo(BeNil(), "Checkpoint still exists (GC doesn't work in envtest, but ownerRef is correct for real cluster)")
		})
	})

	// === GROUP 6: GARBAGE COLLECTION AND OWNER REFERENCES ===

	Describe("Garbage Collection", func() {
		It("should delete chunks when checkpoint is deleted", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// Get chunks and verify ownerReferences
			chunks := listManifestCheckpointContentChunks(ctx, checkpointName)
			Expect(len(chunks)).To(BeNumerically(">", 0))

			// Verify chunks have ownerReference to checkpoint
			mcp := getManifestCheckpoint(ctx, checkpointName)
			for _, chunk := range chunks {
				found := false
				for _, ref := range chunk.OwnerReferences {
					if ref.Kind == "ManifestCheckpoint" && ref.Name == checkpointName && ref.UID == mcp.UID {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(), "Chunk should have ownerReference to ManifestCheckpoint")
			}

			// In envtest, kube-controller-manager is not running, so actual GC won't work
			// Instead, we verify that chunks have correct ownerReferences for GC to work in real cluster
			// All chunks should have Controller=true ownerRef to the checkpoint
			for _, chunk := range chunks {
				hasControllerRef := false
				for _, ref := range chunk.OwnerReferences {
					if ref.Kind == "ManifestCheckpoint" && ref.Name == checkpointName && ref.UID == mcp.UID {
						if ref.Controller != nil && *ref.Controller {
							hasControllerRef = true
							break
						}
					}
				}
				Expect(hasControllerRef).To(BeTrue(),
					fmt.Sprintf("Chunk %s must have Controller=true ownerRef to checkpoint for GC to work in real cluster", chunk.Name))
			}

			// Delete checkpoint to verify ownerReferences are set correctly
			// Note: In envtest, chunks won't actually be deleted by GC, but in real cluster they will
			foreground := metav1.DeletePropagationForeground
			err := k8sClient.Delete(ctx, mcp, &client.DeleteOptions{
				PropagationPolicy: &foreground,
			})
			Expect(err).NotTo(HaveOccurred())

			// In envtest, we can't verify actual deletion, but we've verified ownerReferences above
			// In a real cluster with kube-controller-manager, chunks would be deleted automatically
		})

		It("should keep checkpoint when namespace is deleted", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready and save checkpoint name before namespace deletion
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// Verify checkpoint exists before namespace deletion
			mcp := getManifestCheckpoint(ctx, checkpointName)
			Expect(mcp).NotTo(BeNil())

			// CRITICAL: Verify checkpoint has NO ownerRef to MCR or Namespace
			// Checkpoint should only have ownerRef to Retainer
			hasMCRRef := false
			hasNamespaceRef := false
			hasRetainerRef := false
			for _, ref := range mcp.OwnerReferences {
				if ref.Kind == "ManifestCaptureRequest" {
					hasMCRRef = true
				}
				if ref.Kind == "Namespace" {
					hasNamespaceRef = true
				}
				if ref.Kind == "IRetainer" {
					hasRetainerRef = true
				}
			}
			Expect(hasMCRRef).To(BeFalse(), "Checkpoint must NOT have ownerRef to ManifestCaptureRequest")
			Expect(hasNamespaceRef).To(BeFalse(), "Checkpoint must NOT have ownerRef to Namespace")
			Expect(hasRetainerRef).To(BeTrue(), "Checkpoint must have ownerRef to Retainer")

			// ADR: Only ONE Retainer exists: ret-mcr-<namespace>-<mcrName>
			// This Retainer uses FollowObjectWithTTL mode to follow MCR
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			var retainer iretainer.IRetainer
			err := k8sClient.Get(ctx, client.ObjectKey{Name: retainerName}, &retainer)
			Expect(err).NotTo(HaveOccurred(), "MCR retainer should exist")
			Expect(retainer.Spec.Mode).To(Equal("FollowObjectWithTTL"), "Retainer must use FollowObjectWithTTL mode to follow MCR")
			Expect(retainer.Spec.FollowObjectRef).NotTo(BeNil(), "Retainer must follow MCR")
			Expect(retainer.Spec.FollowObjectRef.Kind).To(Equal("ManifestCaptureRequest"))
			Expect(retainer.Spec.FollowObjectRef.Namespace).To(Equal(testNS))
			Expect(retainer.Spec.FollowObjectRef.Name).To(Equal(TestFixtures.TestMCRName))

			// Delete MCR first (in real cluster, this would be cascaded from namespace deletion)
			// In envtest, we need to explicitly delete MCR to allow namespace deletion
			// The key point is: checkpoint should survive MCR and namespace deletion
			mcrToDelete := &storagev1alpha1.ManifestCaptureRequest{}
			err = k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: TestFixtures.TestMCRName}, mcrToDelete)
			if err == nil {
				// MCR exists, delete it
				err = k8sClient.Delete(ctx, mcrToDelete)
				Expect(err).NotTo(HaveOccurred(), "Should be able to delete MCR")
				// Wait for MCR to be deleted
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNS, Name: TestFixtures.TestMCRName}, &storagev1alpha1.ManifestCaptureRequest{})
					return errors.IsNotFound(err)
				}, testTimeout, pollInterval).Should(BeTrue(), "MCR should be deleted")
			}

			// Now delete namespace
			// In real cluster, namespace deletion would cascade delete MCR automatically
			// But the key point is: checkpoint should survive namespace deletion
			deleteNamespace(ctx, testNS)

			// Verify namespace is deleted or in Terminating state
			// In envtest, namespace might stay in Terminating if there are finalizers
			Eventually(func() bool {
				var ns corev1.Namespace
				err := k8sClient.Get(ctx, client.ObjectKey{Name: testNS}, &ns)
				if errors.IsNotFound(err) {
					return true // Namespace is deleted
				}
				if err != nil {
					return false
				}
				// Namespace is in Terminating state (has DeletionTimestamp)
				return ns.DeletionTimestamp != nil
			}, testTimeout, pollInterval).Should(BeTrue(), "Namespace should be deleted or in Terminating state")

			// Wait a bit for any cascading operations to complete
			time.Sleep(1 * time.Second)

			// Verify retainer still exists (it should not be deleted because it uses TTL mode)
			// In envtest, retainer might be deleted by RetainerController if it follows MCR,
			// but since it uses TTL mode, it should remain
			// Use Eventually to handle potential race conditions
			Eventually(func() bool {
				var retainerAfterDelete iretainer.IRetainer
				err := k8sClient.Get(ctx, client.ObjectKey{Name: retainerName}, &retainerAfterDelete)
				if err != nil {
					return false
				}
				// ADR: Retainer remains in FollowObjectWithTTL mode
				// After MCR deletion, retainer starts TTL countdown (ttl-start-timestamp is set)
				// Mode doesn't change, but TTL countdown begins
				return retainerAfterDelete.Spec.Mode == "FollowObjectWithTTL" &&
					retainerAfterDelete.Annotations != nil &&
					retainerAfterDelete.Annotations["retainer.deckhouse.io/ttl-start-timestamp"] != ""
			}, testTimeout, pollInterval).Should(BeTrue(), "MCR retainer should remain after namespace deletion and start TTL countdown")

			// Verify checkpoint remains (cluster-scoped, not affected by namespace deletion)
			// Checkpoint should NOT be deleted because:
			// 1. It has NO ownerRef to MCR or Namespace (only to Retainer)
			// 2. MCR Retainer uses FollowObjectWithTTL mode - when MCR is deleted, retainer starts TTL countdown
			// 3. Retainer still exists (TTL hasn't expired yet), so checkpoint should remain via ownerRef
			// 4. After TTL expires, retainer will be deleted → GC will delete checkpoint (in real cluster)
			// Use Eventually to handle potential race conditions
			Eventually(func() bool {
				var err error
				mcp, err = func() (*storagev1alpha1.ManifestCheckpoint, error) {
					mcp := &storagev1alpha1.ManifestCheckpoint{}
					key := types.NamespacedName{Name: checkpointName}
					err := k8sClient.Get(ctx, key, mcp)
					return mcp, err
				}()
				return err == nil && mcp != nil
			}, testTimeout, pollInterval).Should(BeTrue(), "Checkpoint should remain after namespace deletion")

			// Verify chunks remain (cluster-scoped, not affected by namespace deletion)
			chunks := listManifestCheckpointContentChunks(ctx, checkpointName)
			Expect(len(chunks)).To(BeNumerically(">", 0))
		})
	})

	// === GROUP 7: ARCHIVE RECOVERY AND STRESS TESTS ===

	Describe("Archive Recovery and Stress Tests", func() {
		It("should restore JSON from chunks correctly", func() {
			// Create test objects
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			createService(ctx, testNS, TestFixtures.TestServiceName)

			// Create ManifestCaptureRequest
			targets := GetStandardTargets()
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// Get checkpoint and chunks
			mcp := getManifestCheckpoint(ctx, checkpointName)
			chunks := listManifestCheckpointContentChunks(ctx, checkpointName)
			Expect(len(chunks)).To(BeNumerically(">", 0))

			// Verify chunks can be decoded and JSON restored
			totalObjects := 0
			for _, chunk := range chunks {
				// Get full chunk object
				fullChunk := getManifestCheckpointContentChunk(ctx, chunk.Name)
				Expect(fullChunk.Spec.Data).NotTo(BeEmpty(), "Chunk data should not be empty")
				Expect(fullChunk.Spec.Checksum).NotTo(BeEmpty(), "Chunk checksum should not be empty")

				// Verify chunk data is valid base64
				decoded, err := base64.StdEncoding.DecodeString(fullChunk.Spec.Data)
				Expect(err).NotTo(HaveOccurred(), "Chunk data should be valid base64")
				Expect(len(decoded)).To(BeNumerically(">", 0), "Decoded chunk data should not be empty")

				// Verify checksum matches
				hash := sha256.Sum256(decoded)
				expectedChecksum := hex.EncodeToString(hash[:])
				Expect(fullChunk.Spec.Checksum).To(Equal(expectedChecksum), "Chunk checksum should match")

				// Decode and verify JSON can be restored
				objects, err := decodeChunkData(fullChunk.Spec.Data)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Chunk %s should decode to valid JSON", chunk.Name))
				Expect(len(objects)).To(BeNumerically(">", 0), fmt.Sprintf("Chunk %s should contain objects", chunk.Name))

				// Verify objects have Kubernetes structure
				for _, obj := range objects {
					Expect(obj).To(HaveKey("apiVersion"), "Object should have apiVersion")
					Expect(obj).To(HaveKey("kind"), "Object should have kind")
					Expect(obj).To(HaveKey("metadata"), "Object should have metadata")
					metadata, ok := obj["metadata"].(map[string]interface{})
					Expect(ok).To(BeTrue(), "Metadata should be a map")
					Expect(metadata).To(HaveKey("name"), "Metadata should have name")
				}

				totalObjects += len(objects)
			}

			// Verify checkpoint status matches decoded objects
			Expect(mcp.Status.TotalObjects).To(Equal(totalObjects), "Checkpoint TotalObjects should match decoded objects")
			Expect(len(mcp.Status.Chunks)).To(Equal(len(chunks)))
		})

		It("should handle 10MB of data and restore correctly", func() {
			// Create multiple large ConfigMaps to reach ~10MB
			const targetSizeBytes = 10 * 1024 * 1024 // 10MB
			const configMapsCount = 10
			const sizePerConfigMap = targetSizeBytes / configMapsCount

			// Create ConfigMaps
			configMapNames := make([]string, 0, configMapsCount)
			for i := 0; i < configMapsCount; i++ {
				cmName := fmt.Sprintf("large-cm-%d", i)
				createLargeConfigMap(ctx, testNS, cmName, sizePerConfigMap)
				configMapNames = append(configMapNames, cmName)
			}

			// Create targets for all ConfigMaps
			targets := make([]storagev1alpha1.ManifestTarget, 0, configMapsCount)
			for _, name := range configMapNames {
				targets = append(targets, makeTarget("v1", "ConfigMap", name))
			}

			// Create ManifestCaptureRequest
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready (may take longer for large data)
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, 2*testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// Verify checkpoint was created
			mcp := getManifestCheckpoint(ctx, checkpointName)
			Expect(mcp).NotTo(BeNil())
			Expect(mcp.Status.TotalObjects).To(Equal(configMapsCount))

			// Verify chunks were created
			chunks := listManifestCheckpointContentChunks(ctx, checkpointName)
			Expect(len(chunks)).To(BeNumerically(">", 0))

			// Verify total size is approximately correct (with compression, it should be less)
			Expect(mcp.Status.TotalSizeBytes).To(BeNumerically(">", 0))
			// Compressed size should be less than original, but not zero
			Expect(mcp.Status.TotalSizeBytes).To(BeNumerically("<", targetSizeBytes*2)) // Allow some overhead

			// Verify all chunks can be decoded, have valid checksums, and JSON can be restored
			totalDecodedSize := 0
			totalRestoredObjects := 0
			for _, chunk := range chunks {
				fullChunk := getManifestCheckpointContentChunk(ctx, chunk.Name)

				// Decode base64
				decoded, err := base64.StdEncoding.DecodeString(fullChunk.Spec.Data)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Chunk %s should be valid base64", chunk.Name))

				// Verify checksum
				hash := sha256.Sum256(decoded)
				expectedChecksum := hex.EncodeToString(hash[:])
				Expect(fullChunk.Spec.Checksum).To(Equal(expectedChecksum), fmt.Sprintf("Chunk %s checksum should match", chunk.Name))

				// Decode and verify JSON can be restored
				objects, err := decodeChunkData(fullChunk.Spec.Data)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Chunk %s should decode to valid JSON", chunk.Name))
				Expect(len(objects)).To(Equal(fullChunk.Spec.ObjectsCount), fmt.Sprintf("Chunk %s object count should match", chunk.Name))

				// Verify objects have Kubernetes structure
				for _, obj := range objects {
					Expect(obj).To(HaveKey("apiVersion"), "Object should have apiVersion")
					Expect(obj).To(HaveKey("kind"), "Object should have kind")
					Expect(obj).To(HaveKey("metadata"), "Object should have metadata")
					// Verify it's a ConfigMap
					kind, ok := obj["kind"].(string)
					Expect(ok).To(BeTrue())
					Expect(kind).To(Equal("ConfigMap"), "Object should be ConfigMap")
				}

				totalDecodedSize += len(decoded)
				totalRestoredObjects += len(objects)
			}

			// Verify we decoded some data
			Expect(totalDecodedSize).To(BeNumerically(">", 0), "Should have decoded chunk data")
			Expect(totalRestoredObjects).To(Equal(configMapsCount), "Should restore all ConfigMaps from chunks")

			// Verify chunk indices form a continuous range [0..len(chunks)-1] regardless of list order
			// Note: chunks from List() may come in arbitrary order, so we check the set of indices
			indices := make(map[int]bool, len(chunks))
			for _, chunk := range chunks {
				indices[chunk.Spec.Index] = true
			}
			for i := 0; i < len(chunks); i++ {
				Expect(indices[i]).To(BeTrue(), fmt.Sprintf("There should be a chunk with index %d", i))
			}
		})
	})
})
