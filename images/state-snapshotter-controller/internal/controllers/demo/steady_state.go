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

package demo

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

// demoSnapshotContentManifestHandoffComplete reports whether manifest capture finished on
// SnapshotContent: non-empty manifestCheckpointName, Ready MCP, Ready content.
// Root E5 exclude reads SnapshotContent/MCP only (not live MCR).
func demoSnapshotContentManifestHandoffComplete(ctx context.Context, c client.Reader, contentName string) (bool, error) {
	content := &storagev1alpha1.SnapshotContent{}
	if err := c.Get(ctx, client.ObjectKey{Name: contentName}, content); err != nil {
		return false, err
	}
	mcpName := content.Status.ManifestCheckpointName
	if mcpName == "" {
		return false, nil
	}
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := c.Get(ctx, client.ObjectKey{Name: mcpName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	readyCond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		return false, nil
	}
	contentReady, _, _, err := commonSnapshotContentReadyForSnapshot(ctx, c, contentName)
	if err != nil {
		return false, err
	}
	return contentReady, nil
}

func demoSnapshotReadyCondition(conditions []metav1.Condition) *metav1.Condition {
	return meta.FindStatusCondition(conditions, snapshot.ConditionReady)
}

func demoSnapshotReadyTrue(conditions []metav1.Condition) bool {
	rc := demoSnapshotReadyCondition(conditions)
	return rc != nil && rc.Status == metav1.ConditionTrue
}

// demoChildManifestCaptureSteadyState is true when the snapshot is Ready and manifest
// handoff is persisted on SnapshotContent. Further reconciles must not recreate MCR.
func demoChildManifestCaptureSteadyState(ctx context.Context, reader client.Reader, conditions []metav1.Condition, contentName string) (bool, error) {
	if !demoSnapshotReadyTrue(conditions) {
		return false, nil
	}
	return demoSnapshotContentManifestHandoffComplete(ctx, reader, contentName)
}

// demoCleanupStrayManifestCaptureRequest removes a completed MCR left over after handoff
// so later reconciles do not observe an empty checkpointName on a live request.
func demoCleanupStrayManifestCaptureRequest(
	ctx context.Context,
	c client.Client,
	reader client.Reader,
	namespace string,
	snapshotKind string,
	snapshotName string,
	contentName string,
) error {
	mcrName := demoSnapshotManifestCaptureRequestName(snapshotKind, namespace, snapshotName)
	key := types.NamespacedName{Namespace: namespace, Name: mcrName}
	mcr := &ssv1alpha1.ManifestCaptureRequest{}
	if err := c.Get(ctx, key, mcr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	safe, err := demoSnapshotManifestCaptureRequestReadyForCleanup(ctx, reader, key, contentName)
	if err != nil {
		return err
	}
	if !safe {
		return nil
	}
	return cleanupDemoSnapshotManifestCaptureRequest(ctx, c, mcr)
}

func demoReconcilerReader(apiReader, fallback client.Reader) client.Reader {
	if apiReader != nil {
		return apiReader
	}
	return fallback
}

// demoReturnIfManifestCaptureSteadyState stops reconcile when manifest handoff is done on
// SnapshotContent so MCR is not recreated and root E5 sees a stable child subtree.
func demoReturnIfManifestCaptureSteadyState(
	ctx context.Context,
	c client.Client,
	reader client.Reader,
	namespace string,
	snapshotKind string,
	snapshotName string,
	conditions []metav1.Condition,
	contentName string,
) (bool, error) {
	steady, err := demoChildManifestCaptureSteadyState(ctx, reader, conditions, contentName)
	if err != nil || !steady {
		return steady, err
	}
	if err := demoCleanupStrayManifestCaptureRequest(ctx, c, reader, namespace, snapshotKind, snapshotName, contentName); err != nil {
		return false, err
	}
	return true, nil
}
