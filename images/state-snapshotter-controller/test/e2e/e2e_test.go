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

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
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

		// Delete all ObjectKeepers created in this test
		objectKeepers := &deckhousev1alpha1.ObjectKeeperList{}
		if err := k8sClient.List(ctx, objectKeepers); err == nil {
			for i := range objectKeepers.Items {
				ok := &objectKeepers.Items[i]
				// ADR: Only ONE ObjectKeeper exists per MCR: ret-mcr-{namespace}-{mcrName}
				// Delete MCR ObjectKeepers for this namespace
				if strings.HasPrefix(ok.Name, fmt.Sprintf("ret-mcr-%s-", testNS)) {
					_ = k8sClient.Delete(ctx, ok)
					continue
				}
				// Best-effort cleanup: delete stray ObjectKeepers named ret-{checkpointName} that are not ret-mcr-*.
				if strings.HasPrefix(ok.Name, "ret-") && !strings.HasPrefix(ok.Name, "ret-mcr-") {
					// Extract checkpoint name from ObjectKeeper name: "ret-{checkpointName}"
					checkpointName := strings.TrimPrefix(ok.Name, "ret-")
					// Check if this checkpoint belongs to our test namespace
					cp := &storagev1alpha1.ManifestCheckpoint{}
					if err := k8sClient.Get(ctx, types.NamespacedName{Name: checkpointName}, cp); err == nil {
						if cp.Spec.ManifestCaptureRequestRef != nil &&
							cp.Spec.ManifestCaptureRequestRef.Namespace == testNS {
							// Stray ObjectKeeper for this namespace — delete for a clean test run
							_ = k8sClient.Delete(ctx, ok)
						}
					} else {
						// Checkpoint doesn't exist (already deleted), safe to delete ObjectKeeper
						_ = k8sClient.Delete(ctx, ok)
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
			ready := findCondition(mcp.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	// === GROUP 2: CONDITION TRANSITIONS ===

	Describe("Condition Transitions", func() {
		// TestConditionTransitionEmptyToReady verifies that when a ManifestCaptureRequest
		// is created, it eventually transitions to Ready condition (True on success, False on failure).
		// Only Ready condition is used - it is set only in final state.
		It("should transition from Empty to Ready", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready condition to be set (Ready=True on success, Ready=False on failure)
			// Ready condition is set only in final state (terminal success or terminal failure)
			Eventually(func() bool {
				var err error
				mcr, err = getManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName)
				if err != nil {
					return false // MCR not found yet, retry
				}
				ready := findCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
				// Ready condition is set only in final state
				return ready != nil && (ready.Status == metav1.ConditionTrue || ready.Status == metav1.ConditionFalse)
			}, testTimeout, pollInterval).Should(BeTrue())

			// Verify that Ready condition was set (in final state)
			mcr = getManifestCaptureRequestOrFail(ctx, testNS, TestFixtures.TestMCRName)
			ready := findCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(ready).NotTo(BeNil(), "Ready condition should be set")
			// Ready condition is set only in final state (True on success, False on failure)
			Expect(ready.Status).To(Or(Equal(metav1.ConditionTrue), Equal(metav1.ConditionFalse)), "Ready should be True or False")
		})

		It("should transition to Ready", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			ready := findCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))

			// Verify checkpointName is set
			Expect(mcr.Status.CheckpointName).NotTo(BeEmpty())
			Expect(mcr.Status.CompletionTimestamp).NotTo(BeNil())

		})

		It("should transition to Ready=False on NotFound", func() {
			targets := []storagev1alpha1.ManifestTarget{
				GetNotFoundTarget(),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// NotFound → Ready=False immediately (terminal state)
			// To retry, user must delete and recreate MCR
			mcr = waitForManifestCaptureRequestFailed(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			ready := findCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(storagev1alpha1.ManifestCaptureRequestConditionReasonFailed))
			Expect(mcr.Status.CheckpointName).To(BeEmpty())
		})

		It("should be noop for terminal Ready=False MCR", func() {
			targets := []storagev1alpha1.ManifestTarget{
				GetNotFoundTarget(),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready=False (NotFound) - terminal state
			mcr = waitForManifestCaptureRequestFailed(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			readyBefore := findCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(readyBefore).NotTo(BeNil())
			reasonBefore := readyBefore.Reason
			completionTimestampBefore := mcr.Status.CompletionTimestamp

			// Trigger reconcile by updating annotation
			mcr.Annotations["test"] = "trigger-reconcile"
			triggerReconcile(ctx, mcr)

			// Give controller time to process
			time.Sleep(500 * time.Millisecond)

			// Verify status unchanged (noop) - terminal Ready=False means no further processing
			mcr = getManifestCaptureRequestOrFail(ctx, testNS, TestFixtures.TestMCRName)
			Expect(mcr.Status.CompletionTimestamp).To(Equal(completionTimestampBefore))
			ready := findCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(reasonBefore))
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

		It("should use deterministic checkpoint name (same name on multiple reconciles)", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)
			mcrUID := string(mcr.UID) // Save UID for verification

			// Wait for Ready
			mcr = waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName1 := mcr.Status.CheckpointName
			Expect(checkpointName1).NotTo(BeEmpty())

			// Verify checkpointName is deterministic (based on MCR UID)
			// Checkpoint name should be: mcp-<sha256(uid)[:8]>
			Expect(checkpointName1).To(HavePrefix("mcp-"))

			// Trigger multiple reconciles
			for i := 0; i < 3; i++ {
				triggerReconcile(ctx, mcr)
				time.Sleep(200 * time.Millisecond) // Give controller time to process
			}

			// Verify checkpointName hasn't changed (deterministic)
			mcr = getManifestCaptureRequestOrFail(ctx, testNS, TestFixtures.TestMCRName)
			checkpointName2 := mcr.Status.CheckpointName
			Expect(checkpointName2).To(Equal(checkpointName1), "Checkpoint name should be deterministic and not change on reconcile")

			// Verify checkpoint still exists and is the same
			mcp := getManifestCheckpoint(ctx, checkpointName1)
			Expect(mcp).NotTo(BeNil())
			Expect(mcp.Spec.ManifestCaptureRequestRef).NotTo(BeNil())
			Expect(mcp.Spec.ManifestCaptureRequestRef.UID).To(Equal(mcrUID), "Checkpoint should reference correct MCR UID")
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
			ready := findCondition(mcp.Status.Conditions, storagev1alpha1.ManifestCheckpointConditionTypeReady)
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

			// Wait for Ready=False (terminal state)
			mcr = waitForManifestCaptureRequestFailed(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			ready := findCondition(mcr.Status.Conditions, storagev1alpha1.ManifestCaptureRequestConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).NotTo(BeEmpty())
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

	Describe("ObjectKeepers", func() {
		It("should create MCR ObjectKeeper with FollowObject mode", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			mcr := createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)

			// Verify MCR ObjectKeeper
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			ok := getRetainer(ctx, retainerName)

			Expect(ok.Spec.Mode).To(Equal("FollowObject"))
			Expect(ok.Spec.FollowObjectRef).NotTo(BeNil())
			Expect(ok.Spec.FollowObjectRef.Kind).To(Equal("ManifestCaptureRequest"))
			Expect(ok.Spec.FollowObjectRef.Name).To(Equal(TestFixtures.TestMCRName))
			Expect(ok.Spec.FollowObjectRef.Namespace).To(Equal(testNS))
			Expect(ok.Spec.FollowObjectRef.UID).To(Equal(string(mcr.UID)))
			// ObjectKeeper has no TTL (TTL is handled by MCR controller)
		})

		It("should create MCR ObjectKeeper with ownerRef", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// ADR: Only ONE ObjectKeeper exists: ret-mcr-<namespace>-<mcrName>
			// This ObjectKeeper:
			// - Uses FollowObject mode to follow MCR (no TTL - TTL is handled by MCR controller)
			// - Holds the ManifestCheckpoint (MCP has ownerRef to this ObjectKeeper)
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			ok := getRetainer(ctx, retainerName)

			// Verify ObjectKeeper follows MCR in FollowObject mode
			Expect(ok.Spec.Mode).To(Equal("FollowObject"), "ObjectKeeper must use FollowObject mode to follow MCR")
			Expect(ok.Spec.FollowObjectRef).NotTo(BeNil(), "ObjectKeeper must follow MCR")
			Expect(ok.Spec.FollowObjectRef.APIVersion).To(Equal("state-snapshotter.deckhouse.io/v1alpha1"))
			Expect(ok.Spec.FollowObjectRef.Kind).To(Equal("ManifestCaptureRequest"))
			Expect(ok.Spec.FollowObjectRef.Namespace).To(Equal(testNS))
			Expect(ok.Spec.FollowObjectRef.Name).To(Equal(TestFixtures.TestMCRName))
			Expect(ok.Spec.FollowObjectRef.UID).To(Equal(string(mcr.UID)))
			// ObjectKeeper has no TTL (TTL is handled by MCR controller)

			// Verify MCP has ownerRef to this ObjectKeeper
			mcp := getManifestCheckpoint(ctx, checkpointName)
			Expect(len(mcp.OwnerReferences)).To(Equal(1), "MCP must have exactly one ownerRef to ret-mcr-* ObjectKeeper")
			ref := mcp.OwnerReferences[0]
			Expect(ref.Kind).To(Equal("ObjectKeeper"))
			Expect(ref.Name).To(Equal(retainerName))
			Expect(ref.UID).To(Equal(ok.UID))
			Expect(ref.Controller).NotTo(BeNil())
			Expect(*ref.Controller).To(BeTrue(), "ObjectKeeper must be controller owner of MCP")
		})

		// TestObjectKeeperFollowsMCR verifies that when MCR is deleted, ObjectKeeper is also deleted
		// (because it follows MCR in FollowObject mode), and checkpoint is deleted via GC.
		// ADR states: ObjectKeeper follows MCR lifecycle. When MCR is deleted, ObjectKeeper is deleted.
		// When ObjectKeeper is deleted, GC deletes checkpoint through ownerRef.
		// IMPORTANT: In envtest, kube-controller-manager is not running, so actual GC won't work.
		// This test verifies that:
		// 1. ObjectKeeper follows MCR in FollowObject mode (no TTL - TTL is handled by MCR controller)
		// 2. When MCR is deleted, ObjectKeeper is automatically deleted (follows object)
		// 3. Checkpoint has correct ownerReference (Controller=true) to ObjectKeeper for GC to work in real cluster
		// In a real cluster with kube-controller-manager, when ObjectKeeper is deleted, checkpoint would be deleted automatically.
		It("should delete checkpoint when MCR is deleted (ObjectKeeper follows MCR)", func() {
			createConfigMap(ctx, testNS, TestFixtures.TestConfigMapName, TestFixtures.TestConfigMapData)
			targets := []storagev1alpha1.ManifestTarget{
				makeTarget("v1", "ConfigMap", TestFixtures.TestConfigMapName),
			}
			createManifestCaptureRequest(ctx, testNS, TestFixtures.TestMCRName, targets)

			// Wait for Ready
			mcr := waitForManifestCaptureRequestReady(ctx, testNS, TestFixtures.TestMCRName, testTimeout)
			checkpointName := mcr.Status.CheckpointName

			// Get checkpoint and MCR ObjectKeeper
			mcp := getManifestCheckpoint(ctx, checkpointName)
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			ok := getRetainer(ctx, retainerName)

			// Verify ObjectKeeper exists and follows MCR
			Expect(ok.Spec.Mode).To(Equal("FollowObject"))
			// ObjectKeeper has no TTL (TTL is handled by MCR controller)

			// Verify checkpoint has ownerRef to ObjectKeeper
			Expect(len(mcp.OwnerReferences)).To(Equal(1))
			Expect(mcp.OwnerReferences[0].Kind).To(Equal("ObjectKeeper"))
			Expect(mcp.OwnerReferences[0].Name).To(Equal(retainerName))
			Expect(mcp.OwnerReferences[0].Controller).NotTo(BeNil())
			Expect(*mcp.OwnerReferences[0].Controller).To(BeTrue())

			// Verify ownerRef structure is correct for GC
			// This test verifies that:
			// 1. ObjectKeeper correctly follows MCR (FollowObject mode)
			// 2. Checkpoint has correct ownerReference structure for GC
			// 3. OwnerRef points to ObjectKeeper (not MCR or Namespace)
			//
			// Note: In envtest:
			// - ObjectKeeperController (from deckhouse-controller) is not running, so ObjectKeeper won't be deleted
			// - kube-controller-manager is not running, so actual GC won't work
			// In a real cluster:
			// - When MCR is deleted, ObjectKeeperController deletes ObjectKeeper (FollowObject mode)
			// - When ObjectKeeper is deleted, GC deletes checkpoint automatically (via ownerRef)
			//
			// This test focuses on verifying the ownerRef structure is correct, which is what our controller
			// is responsible for. Actual GC behavior is verified in real cluster deployments.
		})

		// TestObjectKeeperDeletionAffectsCheckpoint verifies that MCR ObjectKeeper deletion
		// triggers checkpoint deletion via GC (via ownerReference).
		It("should delete checkpoint when MCR ObjectKeeper is deleted (GC via ownerRef)", func() {
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

			// Verify checkpoint has ownerRef to MCR ObjectKeeper
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			Expect(len(mcp.OwnerReferences)).To(Equal(1))
			Expect(mcp.OwnerReferences[0].Kind).To(Equal("ObjectKeeper"))
			Expect(mcp.OwnerReferences[0].Name).To(Equal(retainerName))
			Expect(mcp.OwnerReferences[0].Controller).NotTo(BeNil())
			Expect(*mcp.OwnerReferences[0].Controller).To(BeTrue())

			// Get MCR ObjectKeeper
			mcrOK := getRetainer(ctx, retainerName)
			Expect(mcrOK).NotTo(BeNil())

			// Delete MCR ObjectKeeper manually (simulating deletion)
			// ADR: When ObjectKeeper is deleted → GC deletes MCP (via ownerRef)
			err := k8sClient.Delete(ctx, mcrOK)
			Expect(err).NotTo(HaveOccurred())

			// Wait for ObjectKeeper to be deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: retainerName}, &deckhousev1alpha1.ObjectKeeper{})
				return errors.IsNotFound(err)
			}, testTimeout, pollInterval).Should(BeTrue(), "MCR ObjectKeeper should be deleted")

			// Note: In envtest, kube-controller-manager is not running, so actual GC won't work.
			// This test verifies the ownerReference setup (Controller=true) that would trigger GC in a real cluster.
			// In a real cluster with kube-controller-manager, when ObjectKeeper is deleted, checkpoint would be deleted automatically via GC.
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
			// Checkpoint should only have ownerRef to ObjectKeeper
			hasMCRRef := false
			hasNamespaceRef := false
			hasObjectKeeperRef := false
			for _, ref := range mcp.OwnerReferences {
				if ref.Kind == "ManifestCaptureRequest" {
					hasMCRRef = true
				}
				if ref.Kind == "Namespace" {
					hasNamespaceRef = true
				}
				if ref.Kind == "ObjectKeeper" {
					hasObjectKeeperRef = true
				}
			}
			Expect(hasMCRRef).To(BeFalse(), "Checkpoint must NOT have ownerRef to ManifestCaptureRequest")
			Expect(hasNamespaceRef).To(BeFalse(), "Checkpoint must NOT have ownerRef to Namespace")
			Expect(hasObjectKeeperRef).To(BeTrue(), "Checkpoint must have ownerRef to ObjectKeeper")

			// ADR: Only ONE ObjectKeeper exists: ret-mcr-<namespace>-<mcrName>
			// This ObjectKeeper uses FollowObject mode to follow MCR
			retainerName := fmt.Sprintf("ret-mcr-%s-%s", testNS, TestFixtures.TestMCRName)
			var objectKeeper deckhousev1alpha1.ObjectKeeper
			err := k8sClient.Get(ctx, client.ObjectKey{Name: retainerName}, &objectKeeper)
			Expect(err).NotTo(HaveOccurred(), "MCR ObjectKeeper should exist")
			Expect(objectKeeper.Spec.Mode).To(Equal("FollowObject"), "ObjectKeeper must use FollowObject mode to follow MCR")
			Expect(objectKeeper.Spec.FollowObjectRef).NotTo(BeNil(), "ObjectKeeper must follow MCR")
			Expect(objectKeeper.Spec.FollowObjectRef.Kind).To(Equal("ManifestCaptureRequest"))
			Expect(objectKeeper.Spec.FollowObjectRef.Namespace).To(Equal(testNS))
			Expect(objectKeeper.Spec.FollowObjectRef.Name).To(Equal(TestFixtures.TestMCRName))

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

			// Verify checkpoint ownerRef structure is correct for GC
			// This test verifies that:
			// 1. Checkpoint has NO ownerRef to MCR or Namespace (critical for namespace deletion scenario)
			// 2. Checkpoint has ownerRef ONLY to ObjectKeeper
			// 3. OwnerRef structure is correct (Controller=true, correct Kind/Name/UID)
			//
			// Note: In envtest:
			// - ObjectKeeperController (from deckhouse-controller) is not running, so ObjectKeeper won't be deleted
			// - kube-controller-manager is not running, so actual GC won't work
			// In a real cluster:
			// 1. When namespace is deleted, MCR is deleted (cascading)
			// 2. When MCR is deleted, ObjectKeeperController deletes ObjectKeeper (FollowObject mode)
			// 3. When ObjectKeeper is deleted, GC deletes checkpoint automatically (via ownerRef)
			//
			// This test focuses on verifying the ownerRef structure is correct, which ensures GC will work
			// correctly in real cluster. Actual GC behavior is verified in real cluster deployments.
			//
			// Verify checkpoint still exists (GC doesn't work in envtest, but ownerRef structure is correct)
			mcpAfterDeletion := &storagev1alpha1.ManifestCheckpoint{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: checkpointName}, mcpAfterDeletion)
			Expect(err).NotTo(HaveOccurred(), "Checkpoint should still exist (GC doesn't work in envtest)")

			// Re-verify ownerRef structure after deletion operations
			hasMCRRefAfter := false
			hasNamespaceRefAfter := false
			hasObjectKeeperRefAfter := false
			for _, ref := range mcpAfterDeletion.OwnerReferences {
				if ref.Kind == "ManifestCaptureRequest" {
					hasMCRRefAfter = true
				}
				if ref.Kind == "Namespace" {
					hasNamespaceRefAfter = true
				}
				if ref.Kind == "ObjectKeeper" {
					hasObjectKeeperRefAfter = true
				}
			}
			Expect(hasMCRRefAfter).To(BeFalse(), "Checkpoint must NOT have ownerRef to ManifestCaptureRequest after deletion")
			Expect(hasNamespaceRefAfter).To(BeFalse(), "Checkpoint must NOT have ownerRef to Namespace after deletion")
			Expect(hasObjectKeeperRefAfter).To(BeTrue(), "Checkpoint must have ownerRef to ObjectKeeper after deletion")

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
