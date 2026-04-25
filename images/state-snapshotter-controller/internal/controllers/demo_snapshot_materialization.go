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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

const defaultDemoSnapshotRequeueAfter = 500 * time.Millisecond

func demoSnapshotManifestObjectName(kind, snapshotName string) string {
	sum := sha256.Sum256([]byte(kind + "/" + snapshotName))
	return "demo-snapshot-" + hex.EncodeToString(sum[:8])
}

func demoSnapshotManifestCaptureRequestName(kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(kind + ":" + namespace + "/" + name))
	return "demo-mcr-" + hex.EncodeToString(sum[:10])
}

func ensureDemoSnapshotManifestObject(
	ctx context.Context,
	c client.Client,
	namespace string,
	name string,
	kind string,
	ownerRef metav1.OwnerReference,
) error {
	cmName := demoSnapshotManifestObjectName(kind, name)
	existing := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cmName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Labels: map[string]string{
				"state-snapshotter.deckhouse.io/demo-snapshot-kind": kind,
			},
		},
		Data: map[string]string{
			"snapshot": name,
			"kind":     kind,
		},
	}
	return c.Create(ctx, cm)
}

func ensureDemoSnapshotManifestCaptureRequest(
	ctx context.Context,
	c client.Client,
	namespace string,
	name string,
	kind string,
	ownerRef metav1.OwnerReference,
) (*ssv1alpha1.ManifestCaptureRequest, error) {
	if err := ensureDemoSnapshotManifestObject(ctx, c, namespace, name, kind, ownerRef); err != nil {
		return nil, err
	}
	mcrName := demoSnapshotManifestCaptureRequestName(kind, namespace, name)
	key := types.NamespacedName{Namespace: namespace, Name: mcrName}
	existing := &ssv1alpha1.ManifestCaptureRequest{}
	err := c.Get(ctx, key, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	mcr := &ssv1alpha1.ManifestCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:            mcrName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: ssv1alpha1.ManifestCaptureRequestSpec{
			Targets: []ssv1alpha1.ManifestTarget{{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       demoSnapshotManifestObjectName(kind, name),
			}},
		},
	}
	if err := c.Create(ctx, mcr); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ensureDemoSnapshotManifestCaptureRequest(ctx, c, namespace, name, kind, ownerRef)
		}
		return nil, err
	}
	created := &ssv1alpha1.ManifestCaptureRequest{}
	if err := c.Get(ctx, key, created); err != nil {
		return nil, err
	}
	return created, nil
}

func demoManifestCheckpointReady(
	ctx context.Context,
	c client.Client,
	mcr *ssv1alpha1.ManifestCaptureRequest,
) (mcpName string, ready bool, terminalFailed bool, message string, err error) {
	if mcr.Status.CheckpointName == "" {
		cond := meta.FindStatusCondition(mcr.Status.Conditions, ssv1alpha1.ManifestCaptureRequestConditionTypeReady)
		if cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == ssv1alpha1.ManifestCaptureRequestConditionReasonFailed {
			return "", false, true, cond.Message, nil
		}
		return "", false, false, fmt.Sprintf("waiting for ManifestCaptureRequest %s/%s", mcr.Namespace, mcr.Name), nil
	}

	mcp := &ssv1alpha1.ManifestCheckpoint{}
	if err := c.Get(ctx, client.ObjectKey{Name: mcr.Status.CheckpointName}, mcp); err != nil {
		if apierrors.IsNotFound(err) {
			return mcr.Status.CheckpointName, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %q", mcr.Status.CheckpointName), nil
		}
		return "", false, false, "", err
	}
	cond := meta.FindStatusCondition(mcp.Status.Conditions, ssv1alpha1.ManifestCheckpointConditionTypeReady)
	if cond == nil {
		return mcp.Name, false, false, fmt.Sprintf("waiting for ManifestCheckpoint %q Ready condition", mcp.Name), nil
	}
	if cond.Status == metav1.ConditionTrue {
		return mcp.Name, true, false, cond.Message, nil
	}
	if cond.Reason == ssv1alpha1.ManifestCheckpointConditionReasonFailed {
		return mcp.Name, false, true, cond.Message, nil
	}
	return mcp.Name, false, false, cond.Message, nil
}

func demoSnapshotOwnerReference(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        uid,
	}
}
