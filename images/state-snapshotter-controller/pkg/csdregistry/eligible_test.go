package csdregistry

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func TestCSDWatchEligible(t *testing.T) {
	gen := int64(3)
	d := &storagev1alpha1.CustomSnapshotDefinition{
		ObjectMeta: metav1.ObjectMeta{Generation: gen},
		Status: storagev1alpha1.CustomSnapshotDefinitionStatus{
			Conditions: []metav1.Condition{
				{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: gen},
				{Type: "RBACReady", Status: metav1.ConditionTrue, ObservedGeneration: gen},
			},
		},
	}
	if !CSDWatchEligible(d) {
		t.Fatal("expected eligible")
	}
	d.Status.Conditions[1].ObservedGeneration = 1
	if CSDWatchEligible(d) {
		t.Fatal("expected not eligible when RBACReady generation stale")
	}
}

func TestCSDWatchEligible_nil(t *testing.T) {
	if CSDWatchEligible(nil) {
		t.Fatal("nil should not be eligible")
	}
}

func TestCSDWatchEligible_missingRBAC(t *testing.T) {
	gen := int64(1)
	d := &storagev1alpha1.CustomSnapshotDefinition{
		ObjectMeta: metav1.ObjectMeta{Generation: gen},
		Status: storagev1alpha1.CustomSnapshotDefinitionStatus{
			Conditions: []metav1.Condition{
				{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: gen, LastTransitionTime: metav1.Now()},
			},
		},
	}
	if CSDWatchEligible(d) {
		t.Fatal("missing RBACReady")
	}
}
