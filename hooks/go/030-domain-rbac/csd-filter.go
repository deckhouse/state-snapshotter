package domain_rbac

import (
	"github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/hooks/go/consts"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FilterAcceptedCSD returns items where Accepted=True at the current metadata.generation.
func filterAcceptedCSD(items []v1alpha1.CustomSnapshotDefinition) []v1alpha1.CustomSnapshotDefinition {
	out := make([]v1alpha1.CustomSnapshotDefinition, 0, len(items))

	for _, d := range items {
		cond := apimeta.FindStatusCondition(d.Status.Conditions, consts.CSDConditionAccepted)
		if cond != nil && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == d.Generation {
			out = append(out, d)
		}
	}

	return out
}
