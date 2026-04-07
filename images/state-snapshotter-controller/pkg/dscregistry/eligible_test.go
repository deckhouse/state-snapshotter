package dscregistry

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func TestDSCWatchEligible(t *testing.T) {
	gen := int64(3)
	d := &storagev1alpha1.DomainSpecificSnapshotController{
		ObjectMeta: metav1.ObjectMeta{Generation: gen},
		Status: storagev1alpha1.DomainSpecificSnapshotControllerStatus{
			Conditions: []metav1.Condition{
				{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: gen},
				{Type: "RBACReady", Status: metav1.ConditionTrue, ObservedGeneration: gen},
			},
		},
	}
	if !DSCWatchEligible(d) {
		t.Fatal("expected eligible")
	}
	d.Status.Conditions[1].ObservedGeneration = 1
	if DSCWatchEligible(d) {
		t.Fatal("expected not eligible when RBACReady generation stale")
	}
}

func TestDSCWatchEligible_nil(t *testing.T) {
	if DSCWatchEligible(nil) {
		t.Fatal("nil should not be eligible")
	}
}

func TestDSCWatchEligible_missingRBAC(t *testing.T) {
	gen := int64(1)
	d := &storagev1alpha1.DomainSpecificSnapshotController{
		ObjectMeta: metav1.ObjectMeta{Generation: gen},
		Status: storagev1alpha1.DomainSpecificSnapshotControllerStatus{
			Conditions: []metav1.Condition{
				{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: gen, LastTransitionTime: metav1.Now()},
			},
		},
	}
	if DSCWatchEligible(d) {
		t.Fatal("missing RBACReady")
	}
}
