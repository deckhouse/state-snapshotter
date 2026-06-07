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

package snapshotcontent

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/pkg/snapshot"
)

const (
	kindVolumeSnapshotContent       = "VolumeSnapshotContent"
	volumeSnapshotContentAPIVersion = "snapshot.storage.k8s.io/v1"
)

// Execution requests must not appear in published dataRefs[].artifact.
var dataArtifactExecutionRequestKinds = map[string]struct{}{
	"VolumeCaptureRequest":   {},
	"ManifestCaptureRequest": {},
	"VolumeSnapshot":         {},
	"DataExport":             {},
	"DataImport":             {},
}

// resolveDataReadiness validates status.dataRefs[] for common SnapshotContent Ready aggregation.
// Empty or nil dataRefs[] means the data leg is N/A (manifest-only). Does not create VCR/MCR or publish refs.
func (r *SnapshotContentController) resolveDataReadiness(ctx context.Context, obj *unstructured.Unstructured) (bool, string, string, error) {
	contentLike, err := snapshot.ExtractSnapshotContentLike(obj)
	if err != nil {
		return false, snapshot.ReasonDataArtifactInvalid, "failed to read SnapshotContent status", err
	}

	refs := contentLike.GetStatusDataRefs()
	if len(refs) == 0 {
		return true, "", "", nil
	}

	var invalid []string
	var unsupported []string
	var missing []string
	var notReady []string

	for _, binding := range refs {
		ready, reason, msg, err := r.resolveOneDataBindingReadiness(ctx, binding)
		if err != nil {
			return false, reason, msg, err
		}
		if ready {
			continue
		}
		switch reason {
		case snapshot.ReasonDataArtifactInvalid:
			invalid = append(invalid, msg)
		case snapshot.ReasonDataArtifactNotSupported:
			unsupported = append(unsupported, msg)
		case snapshot.ReasonArtifactMissing:
			if binding.Artifact.Name != "" {
				missing = append(missing, binding.Artifact.Name)
			} else {
				missing = append(missing, msg)
			}
		case snapshot.ReasonArtifactNotReady:
			if binding.Artifact.Name != "" {
				notReady = append(notReady, binding.Artifact.Name)
			} else {
				notReady = append(notReady, msg)
			}
		default:
			notReady = append(notReady, msg)
		}
	}

	if len(invalid) > 0 {
		return false, snapshot.ReasonDataArtifactInvalid, strings.Join(invalid, "; "), nil
	}
	if len(unsupported) > 0 {
		return false, snapshot.ReasonDataArtifactNotSupported, strings.Join(unsupported, "; "), nil
	}
	if len(missing) > 0 {
		return false, snapshot.ReasonArtifactMissing,
			fmt.Sprintf("data artifact(s) missing: %s", strings.Join(missing, ", ")), nil
	}
	if len(notReady) > 0 {
		// Surface the data leg pending state as DataCapturePending with a progress count. When this branch
		// is reached the terminal buckets (invalid/unsupported/missing) are empty, so ready = total - notReady.
		ready := len(refs) - len(notReady)
		return false, snapshot.ReasonDataCapturePending,
			"waiting for volume snapshot artifacts: " + formatReadyProgress(ready, len(refs), notReady), nil
	}

	return true, "", "", nil
}

func (r *SnapshotContentController) resolveOneDataBindingReadiness(
	ctx context.Context,
	binding snapshot.DataBindingRef,
) (bool, string, string, error) {
	art := binding.Artifact
	targetKey := binding.TargetUID
	if targetKey == "" {
		targetKey = art.Name
	}

	if art.APIVersion == "" || art.Kind == "" || art.Name == "" {
		return false, snapshot.ReasonDataArtifactInvalid,
			fmt.Sprintf("dataRefs artifact must set apiVersion, kind, and name (targetUID=%s)", targetKey), nil
	}
	if _, isRequest := dataArtifactExecutionRequestKinds[art.Kind]; isRequest {
		return false, snapshot.ReasonDataArtifactNotSupported,
			fmt.Sprintf("execution request %s is not a durable data artifact (targetUID=%s)", art.Kind, targetKey), nil
	}
	if art.Kind != kindVolumeSnapshotContent {
		return false, snapshot.ReasonDataArtifactNotSupported,
			fmt.Sprintf("unsupported data artifact kind %s (targetUID=%s)", art.Kind, targetKey), nil
	}
	if art.APIVersion != volumeSnapshotContentAPIVersion {
		return false, snapshot.ReasonDataArtifactNotSupported,
			fmt.Sprintf("unsupported VolumeSnapshotContent apiVersion %s (targetUID=%s)", art.APIVersion, targetKey), nil
	}

	return r.checkVolumeSnapshotContentReadiness(ctx, art.Name)
}

func (r *SnapshotContentController) checkVolumeSnapshotContentReadiness(
	ctx context.Context,
	name string,
) (bool, string, string, error) {
	gvk := artifactGVK(volumeSnapshotContentAPIVersion, kindVolumeSnapshotContent)
	artifactObj := &unstructured.Unstructured{}
	artifactObj.SetGroupVersionKind(gvk)

	err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, artifactObj)
	if errors.IsNotFound(err) {
		return false, snapshot.ReasonArtifactMissing,
			fmt.Sprintf("VolumeSnapshotContent %s not found", name), nil
	}
	if err != nil {
		return false, "", "", fmt.Errorf("get VolumeSnapshotContent %s: %w", name, err)
	}

	readyToUse, found, err := unstructured.NestedBool(artifactObj.Object, "status", "readyToUse")
	if err != nil {
		return false, "", "", fmt.Errorf("read VolumeSnapshotContent %s status.readyToUse: %w", name, err)
	}
	if !found || !readyToUse {
		return false, snapshot.ReasonArtifactNotReady,
			fmt.Sprintf("VolumeSnapshotContent %s is not readyToUse", name), nil
	}

	return true, "", "", nil
}

func artifactGVK(apiVersion, kind string) schema.GroupVersionKind {
	if idx := strings.Index(apiVersion, "/"); idx != -1 {
		return schema.GroupVersionKind{
			Group:   apiVersion[:idx],
			Version: apiVersion[idx+1:],
			Kind:    kind,
		}
	}
	return schema.GroupVersionKind{
		Group:   "",
		Version: apiVersion,
		Kind:    kind,
	}
}
