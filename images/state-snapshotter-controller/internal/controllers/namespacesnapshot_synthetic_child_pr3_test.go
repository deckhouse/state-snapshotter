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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

func TestEvaluateSyntheticRequiredChildStateForPR2(t *testing.T) {
	t.Parallel()
	ns, name := "ns1", "parent-child"
	base := func() *storagev1alpha1.NamespaceSnapshot {
		return &storagev1alpha1.NamespaceSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Status: storagev1alpha1.NamespaceSnapshotStatus{
				BoundSnapshotContentName: "nsc-child-1",
			},
		}
	}

	t.Run("unbound pending", func(t *testing.T) {
		t.Parallel()
		ch := base()
		ch.Status.BoundSnapshotContentName = ""
		got := evaluateSyntheticRequiredChildStateForPR2(ch)
		if got.Phase != syntheticChildAggregatePending || got.Reason != snapshot.ReasonChildSnapshotPending {
			t.Fatalf("got %+v", got)
		}
		if got.Message == "" {
			t.Fatal("empty message")
		}
	})

	t.Run("no ready condition pending", func(t *testing.T) {
		t.Parallel()
		got := evaluateSyntheticRequiredChildStateForPR2(base())
		if got.Phase != syntheticChildAggregatePending {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("ready true", func(t *testing.T) {
		t.Parallel()
		ch := base()
		meta.SetStatusCondition(&ch.Status.Conditions, metav1.Condition{
			Type:   snapshot.ConditionReady,
			Status: metav1.ConditionTrue,
			Reason: snapshot.ReasonCompleted,
		})
		got := evaluateSyntheticRequiredChildStateForPR2(ch)
		if got.Phase != syntheticChildAggregateReady || got.Reason != "" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("ready false manifest checkpoint pending is not terminal", func(t *testing.T) {
		t.Parallel()
		ch := base()
		meta.SetStatusCondition(&ch.Status.Conditions, metav1.Condition{
			Type:    snapshot.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ManifestCheckpointPending",
			Message: "waiting for ManifestCheckpoint \"mcp-123\"",
		})
		got := evaluateSyntheticRequiredChildStateForPR2(ch)
		if got.Phase != syntheticChildAggregatePending || got.Reason != snapshot.ReasonChildSnapshotPending {
			t.Fatalf("got %+v", got)
		}
		if !strings.Contains(got.Message, "ManifestCheckpointPending") || !strings.Contains(got.Message, "mcp-123") {
			t.Fatalf("pending message should carry child reason and message: %q", got.Message)
		}
	})

	t.Run("ready false no capture targets is terminal", func(t *testing.T) {
		t.Parallel()
		ch := base()
		meta.SetStatusCondition(&ch.Status.Conditions, metav1.Condition{
			Type:    snapshot.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "NoCaptureTargets",
			Message: "no targets",
		})
		got := evaluateSyntheticRequiredChildStateForPR2(ch)
		if got.Phase != syntheticChildAggregateFailed || got.Reason != snapshot.ReasonChildSnapshotFailed {
			t.Fatalf("got %+v", got)
		}
		if got.Message == "" {
			t.Fatal("empty message")
		}
	})

	t.Run("ready false unknown reason stays pending", func(t *testing.T) {
		t.Parallel()
		ch := base()
		meta.SetStatusCondition(&ch.Status.Conditions, metav1.Condition{
			Type:   snapshot.ConditionReady,
			Status: metav1.ConditionFalse,
			Reason: "SomeFutureReason",
		})
		got := evaluateSyntheticRequiredChildStateForPR2(ch)
		if got.Phase != syntheticChildAggregatePending {
			t.Fatalf("got %+v", got)
		}
	})
}
