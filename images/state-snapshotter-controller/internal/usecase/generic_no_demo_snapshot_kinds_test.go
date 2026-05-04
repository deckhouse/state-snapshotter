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

package usecase

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Demo snapshot CRD identifiers must not appear in non-test .go sources: generic usecase stays
// abstract (GVKRegistry + unstructured); demo domain lives under domain controllers and integration tests.
func TestProductionSourcesDoNotNameDemoSnapshotKinds(t *testing.T) {
	t.Helper()
	forbidden := []string{
		"DemoVirtualDiskSnapshot",
		"DemoVirtualMachineSnapshot",
		"DemoVirtualDiskSnapshot" + "Content",
		"DemoVirtualMachineSnapshot" + "Content",
	}
	forbiddenImports := []string{
		"github.com/deckhouse/state-snapshotter/api/demo",
	}
	roots := []string{
		".",
		filepath.Join("..", "controllers"),
		filepath.Join("..", "api"),
		filepath.Join("..", "..", "pkg", "snapshotgraphregistry"),
	}
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info.IsDir() {
				return walkErr
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if strings.Contains(path, string(filepath.Separator)+"controllers"+string(filepath.Separator)) {
				base := filepath.Base(path)
				if !strings.HasPrefix(base, "namespacesnapshot_") {
					return nil
				}
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, f := range forbidden {
				if bytes.Contains(b, []byte(f)) {
					t.Errorf("%s must not contain demo-only snapshot identifier %q", path, f)
				}
			}
			for _, imp := range forbiddenImports {
				if bytes.Contains(b, []byte(imp)) {
					t.Errorf("%s must not import demo API %q (keep generic layers domain-free)", path, imp)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
