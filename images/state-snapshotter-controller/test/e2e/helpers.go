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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

// createNamespace creates a test namespace
func createNamespace(ctx context.Context, name string) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	err := k8sClient.Create(ctx, ns)
	Expect(err).NotTo(HaveOccurred())
	return ns
}

// deleteNamespace deletes a test namespace
func deleteNamespace(ctx context.Context, name string) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	err := k8sClient.Delete(ctx, ns)
	Expect(err).NotTo(HaveOccurred())
}

// createConfigMap creates a test ConfigMap
func createConfigMap(ctx context.Context, namespace, name string, data map[string]string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
	err := k8sClient.Create(ctx, cm)
	Expect(err).NotTo(HaveOccurred())
	return cm
}

// createService creates a test Service
func createService(ctx context.Context, namespace, name string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Port: 80,
				},
			},
		},
	}
	err := k8sClient.Create(ctx, svc)
	Expect(err).NotTo(HaveOccurred())
	return svc
}

// createManifestCaptureRequest creates a ManifestCaptureRequest with given targets
func createManifestCaptureRequest(ctx context.Context, namespace, name string, targets []storagev1alpha1.ManifestTarget) *storagev1alpha1.ManifestCaptureRequest {
	mcr := &storagev1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: storagev1alpha1.ManifestCaptureRequestSpec{
			Targets: targets,
		},
	}
	err := k8sClient.Create(ctx, mcr)
	Expect(err).NotTo(HaveOccurred())
	return mcr
}

// getManifestCaptureRequest gets a ManifestCaptureRequest
// Returns error if not found (for use in Eventually blocks)
func getManifestCaptureRequest(ctx context.Context, namespace, name string) (*storagev1alpha1.ManifestCaptureRequest, error) {
	mcr := &storagev1alpha1.ManifestCaptureRequest{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	err := k8sClient.Get(ctx, key, mcr)
	if err != nil {
		return nil, err
	}
	return mcr, nil
}

// getManifestCaptureRequestOrFail gets a ManifestCaptureRequest and fails if not found
// Use this when you expect the MCR to exist (outside of Eventually blocks)
func getManifestCaptureRequestOrFail(ctx context.Context, namespace, name string) *storagev1alpha1.ManifestCaptureRequest {
	mcr, err := getManifestCaptureRequest(ctx, namespace, name)
	Expect(err).NotTo(HaveOccurred())
	return mcr
}

// getManifestCheckpoint gets a ManifestCheckpoint
func getManifestCheckpoint(ctx context.Context, name string) *storagev1alpha1.ManifestCheckpoint {
	mcp := &storagev1alpha1.ManifestCheckpoint{}
	key := types.NamespacedName{Name: name}
	err := k8sClient.Get(ctx, key, mcp)
	Expect(err).NotTo(HaveOccurred())
	return mcp
}

// getManifestCheckpointContentChunk gets a ManifestCheckpointContentChunk
func getManifestCheckpointContentChunk(ctx context.Context, name string) *storagev1alpha1.ManifestCheckpointContentChunk {
	chunk := &storagev1alpha1.ManifestCheckpointContentChunk{}
	key := types.NamespacedName{Name: name}
	err := k8sClient.Get(ctx, key, chunk)
	Expect(err).NotTo(HaveOccurred())
	return chunk
}

// getObjectKeeper gets an ObjectKeeper
func getObjectKeeper(ctx context.Context, name string) *deckhousev1alpha1.ObjectKeeper {
	ok := &deckhousev1alpha1.ObjectKeeper{}
	key := types.NamespacedName{Name: name}
	err := k8sClient.Get(ctx, key, ok)
	Expect(err).NotTo(HaveOccurred())
	return ok
}

// getRetainer is an alias for getObjectKeeper (for backward compatibility in tests)
func getRetainer(ctx context.Context, name string) *deckhousev1alpha1.ObjectKeeper {
	return getObjectKeeper(ctx, name)
}

// createObjectKeeper creates an ObjectKeeper manually (for testing migration scenarios)
func createObjectKeeper(ctx context.Context, name string, mode string, followObjectRef *deckhousev1alpha1.FollowObjectRef) *deckhousev1alpha1.ObjectKeeper {
	ok := &deckhousev1alpha1.ObjectKeeper{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "deckhouse.io/v1alpha1",
			Kind:       "ObjectKeeper",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: deckhousev1alpha1.ObjectKeeperSpec{
			Mode:            mode,
			FollowObjectRef: followObjectRef,
		},
	}
	err := k8sClient.Create(ctx, ok)
	Expect(err).NotTo(HaveOccurred())
	return ok
}

// listManifestCheckpointContentChunks lists all chunks for a checkpoint
func listManifestCheckpointContentChunks(ctx context.Context, checkpointName string) []storagev1alpha1.ManifestCheckpointContentChunk {
	chunks := &storagev1alpha1.ManifestCheckpointContentChunkList{}
	err := k8sClient.List(ctx, chunks, client.MatchingLabels{})
	Expect(err).NotTo(HaveOccurred())

	var result []storagev1alpha1.ManifestCheckpointContentChunk
	for _, chunk := range chunks.Items {
		if chunk.Spec.CheckpointName == checkpointName {
			result = append(result, chunk)
		}
	}
	return result
}

// waitForManifestCaptureRequestReady waits for MCR to become Ready=True (success state)
// Only Ready condition is used - Ready=True indicates successful completion
func waitForManifestCaptureRequestReady(ctx context.Context, namespace, name string, timeout time.Duration) *storagev1alpha1.ManifestCaptureRequest {
	var mcr *storagev1alpha1.ManifestCaptureRequest
	Eventually(func() bool {
		var err error
		mcr, err = getManifestCaptureRequest(ctx, namespace, name)
		if err != nil {
			return false // MCR not found yet, retry
		}
		ready := findCondition(mcr.Status.Conditions, storagev1alpha1.ConditionTypeReady)
		// Ready=True indicates successful completion
		return ready != nil && ready.Status == metav1.ConditionTrue
	}, timeout, 100*time.Millisecond).Should(BeTrue())
	return mcr
}

// waitForManifestCaptureRequestFailed waits for MCR to become Ready=False (terminal state)
// Ready=False indicates terminal failure - no retry, operation is complete
func waitForManifestCaptureRequestFailed(ctx context.Context, namespace, name string, timeout time.Duration) *storagev1alpha1.ManifestCaptureRequest {
	var mcr *storagev1alpha1.ManifestCaptureRequest
	Eventually(func() bool {
		var err error
		mcr, err = getManifestCaptureRequest(ctx, namespace, name)
		if err != nil {
			return false // MCR not found yet, retry
		}
		ready := findCondition(mcr.Status.Conditions, storagev1alpha1.ConditionTypeReady)
		// Ready=False indicates terminal failure
		return ready != nil && ready.Status == metav1.ConditionFalse
	}, timeout, 100*time.Millisecond).Should(BeTrue())
	return mcr
}

// waitForManifestCheckpointReady waits for MCP to become Ready=True (success state)
// Only Ready condition is used - Ready=True indicates successful completion
func waitForManifestCheckpointReady(ctx context.Context, name string, timeout time.Duration) *storagev1alpha1.ManifestCheckpoint {
	var mcp *storagev1alpha1.ManifestCheckpoint
	Eventually(func() bool {
		mcp = getManifestCheckpoint(ctx, name)
		ready := findCondition(mcp.Status.Conditions, storagev1alpha1.ConditionTypeReady)
		// Ready=True indicates successful completion
		return ready != nil && ready.Status == metav1.ConditionTrue
	}, timeout, 100*time.Millisecond).Should(BeTrue())
	return mcp
}

// findCondition finds a condition by type
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// triggerReconcile triggers a reconcile by updating the resource
func triggerReconcile(ctx context.Context, obj client.Object) {
	err := k8sClient.Update(ctx, obj)
	Expect(err).NotTo(HaveOccurred())
	// Give controller time to process
	time.Sleep(100 * time.Millisecond)
}

// waitForDeletion waits for an object to be deleted
func waitForDeletion(ctx context.Context, obj client.Object, timeout time.Duration) {
	Eventually(func() bool {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		return errors.IsNotFound(err)
	}, timeout, 100*time.Millisecond).Should(BeTrue())
}

// countObjectKeepers counts all ObjectKeepers
func countObjectKeepers(ctx context.Context) int {
	objectKeepers := &deckhousev1alpha1.ObjectKeeperList{}
	err := k8sClient.List(ctx, objectKeepers)
	Expect(err).NotTo(HaveOccurred())
	return len(objectKeepers.Items)
}

// countRetainers is an alias for countObjectKeepers (for backward compatibility in tests)
func countRetainers(ctx context.Context) int {
	return countObjectKeepers(ctx)
}

// makeTarget creates a ManifestTarget
func makeTarget(apiVersion, kind, name string) storagev1alpha1.ManifestTarget {
	return storagev1alpha1.ManifestTarget{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
	}
}

// verifyCheckpointIsClusterScoped verifies that checkpoint has no namespace
func verifyCheckpointIsClusterScoped(mcp *storagev1alpha1.ManifestCheckpoint) {
	Expect(mcp.Namespace).To(BeEmpty(), fmt.Sprintf("ManifestCheckpoint %s should be cluster-scoped (no namespace)", mcp.Name))
}

// verifyChunkIsClusterScoped verifies that chunk has no namespace
func verifyChunkIsClusterScoped(chunk *storagev1alpha1.ManifestCheckpointContentChunk) {
	Expect(chunk.Namespace).To(BeEmpty(), fmt.Sprintf("ManifestCheckpointContentChunk %s should be cluster-scoped (no namespace)", chunk.Name))
}

// createLargeConfigMap creates a ConfigMap with large data (for stress testing)
func createLargeConfigMap(ctx context.Context, namespace, name string, sizeBytes int) *corev1.ConfigMap {
	// Generate data to reach target size
	// Each key-value pair is approximately: "key-{i}": "value-{i}" + overhead
	// Estimate: ~50 bytes per entry (key + value + overhead)
	entriesNeeded := sizeBytes / 50
	if entriesNeeded < 1 {
		entriesNeeded = 1
	}

	data := make(map[string]string)
	currentSize := 0
	for i := 0; i < entriesNeeded && currentSize < sizeBytes; i++ {
		key := fmt.Sprintf("key-%d", i)
		// Generate value to fill remaining space
		valueSize := (sizeBytes - currentSize) / (entriesNeeded - i)
		if valueSize < 30 {
			valueSize = 30 // Ensure enough space for "value-{i}-" prefix (at least 20 chars)
		}
		randomPartSize := valueSize - 20 // Subtract prefix "value-{i}-" length
		if randomPartSize < 0 {
			randomPartSize = 0
		}
		value := fmt.Sprintf("value-%d-%s", i, generateRandomString(randomPartSize))
		data[key] = value
		currentSize += len(key) + len(value) + 10 // +10 for overhead
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
	err := k8sClient.Create(ctx, cm)
	Expect(err).NotTo(HaveOccurred())
	return cm
}

// generateRandomString generates a random string of specified length
func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[i%len(charset)]
	}
	return string(b)
}

// decodeChunkData decodes base64(gzip(json[])) chunk data and returns JSON objects
// This is a helper for testing chunk restoration
func decodeChunkData(encodedData string) ([]map[string]interface{}, error) {
	// Decode base64
	data, err := base64.StdEncoding.DecodeString(encodedData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}

	// Decompress gzip
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gr.Close()

	decompressed, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress: %w", err)
	}

	// Parse JSON array
	var jsonArray []interface{}
	if err := json.Unmarshal(decompressed, &jsonArray); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON array: %w", err)
	}

	// Convert to map[string]interface{}
	objects := make([]map[string]interface{}, 0, len(jsonArray))
	for _, item := range jsonArray {
		objMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		objects = append(objects, objMap)
	}

	return objects, nil
}
