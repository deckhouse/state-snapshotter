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

package manifestcapture

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
)

func checkpointWithMCRRef(name, namespace string) *storagev1alpha1.ManifestCheckpoint {
	cp := &storagev1alpha1.ManifestCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "cp"},
	}
	if name != "" || namespace != "" {
		cp.Spec.ManifestCaptureRequestRef = &storagev1alpha1.ObjectReference{Name: name, Namespace: namespace}
	}
	return cp
}

func TestMapManifestCheckpointToMCR(t *testing.T) {
	tests := []struct {
		name     string
		obj      client.Object
		wantName string
		wantNS   string
		wantLen  int
	}{
		{
			name:     "valid back-reference enqueues the owning MCR",
			obj:      checkpointWithMCRRef("mcr-1", "ns-a"),
			wantName: "mcr-1",
			wantNS:   "ns-a",
			wantLen:  1,
		},
		{
			name:    "nil ref -> no enqueue",
			obj:     checkpointWithMCRRef("", ""),
			wantLen: 0,
		},
		{
			name:    "missing name -> no enqueue",
			obj:     checkpointWithMCRRef("", "ns-a"),
			wantLen: 0,
		},
		{
			name:    "missing namespace -> no enqueue",
			obj:     checkpointWithMCRRef("mcr-1", ""),
			wantLen: 0,
		},
		{
			name:    "wrong type -> no enqueue",
			obj:     &storagev1alpha1.ManifestCaptureRequest{},
			wantLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs := mapManifestCheckpointToMCR(context.Background(), tt.obj)
			if len(reqs) != tt.wantLen {
				t.Fatalf("got %d requests, want %d", len(reqs), tt.wantLen)
			}
			if tt.wantLen == 1 {
				if reqs[0].Name != tt.wantName || reqs[0].Namespace != tt.wantNS {
					t.Fatalf("got %s/%s, want %s/%s", reqs[0].Namespace, reqs[0].Name, tt.wantNS, tt.wantName)
				}
			}
		})
	}
}
