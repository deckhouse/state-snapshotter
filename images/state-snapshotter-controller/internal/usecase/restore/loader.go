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

package restore

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
)

type Loader struct {
	client         client.Client
	archiveService *usecase.ArchiveService
}

func NewLoader(client client.Client, archiveService *usecase.ArchiveService) *Loader {
	return &Loader{client: client, archiveService: archiveService}
}

func (l *Loader) LoadManifests(ctx context.Context, checkpointName string) ([]unstructured.Unstructured, error) {
	checkpoint := &storagev1alpha1.ManifestCheckpoint{}
	if err := l.client.Get(ctx, types.NamespacedName{Name: checkpointName}, checkpoint); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("checkpoint %s not found", checkpointName)
		}
		return nil, fmt.Errorf("failed to get checkpoint: %w", err)
	}

	req := &usecase.ArchiveRequest{
		CheckpointName: checkpointName,
		CheckpointUID:  string(checkpoint.UID),
	}
	archiveData, _, err := l.archiveService.GetArchiveFromCheckpoint(ctx, checkpoint, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get archive: %w", err)
	}

	var rawObjects []map[string]interface{}
	if err := json.Unmarshal(archiveData, &rawObjects); err != nil {
		return nil, fmt.Errorf("failed to decode archive: %w", err)
	}

	objects := make([]unstructured.Unstructured, 0, len(rawObjects))
	for _, raw := range rawObjects {
		obj := unstructured.Unstructured{Object: raw}
		objects = append(objects, obj)
	}
	return objects, nil
}
