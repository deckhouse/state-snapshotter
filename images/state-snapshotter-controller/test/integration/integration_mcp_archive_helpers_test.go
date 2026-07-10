//go:build integration
// +build integration

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

package integration

import (
	"context"
	"encoding/json"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/usecase"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
)

// integrationArchiveObjectsFromMCP decodes the manifest archive bytes that the archive service builds
// for a given ManifestCheckpoint. Kind-agnostic helper shared by core envtest specs.
func integrationArchiveObjectsFromMCP(ctx context.Context, mcpName string) []map[string]interface{} {
	log, err := logger.NewLogger("error")
	Expect(err).NotTo(HaveOccurred())
	arch := usecase.NewArchiveService(k8sClient, k8sClient, log)
	mcp := &ssv1alpha1.ManifestCheckpoint{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: mcpName}, mcp)).To(Succeed())
	raw, _, err := arch.GetArchiveFromCheckpoint(ctx, mcp, &usecase.ArchiveRequest{
		CheckpointName: mcpName,
		CheckpointUID:  string(mcp.UID),
	})
	Expect(err).NotTo(HaveOccurred())
	var objects []map[string]interface{}
	Expect(json.Unmarshal(raw, &objects)).To(Succeed())
	return objects
}
