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

package genericbinder

import (
	"os"
	"strings"
	"testing"
)

// TestBinderNeverReferencesCommonControllerStatusKey pins the pure-creator invariant (content-single-writer
// design decision #10): the GenericSnapshotBinderController never reads or writes the main-owned
// status.captureState.commonController half. The mechanical proxy for that invariant is the absence of the
// quoted "commonController" status-key literal in the package's non-test source — every commonController
// access goes through an unstructured path built from that string, so a regression that reintroduces one
// necessarily adds the literal back. Prose mentions of commonController in comments are unquoted and do not
// trip this guard. Kept dependency-free (os.ReadDir over the package dir the test runs in).
func TestBinderNeverReferencesCommonControllerStatusKey(t *testing.T) {
	const forbidden = `"commonController"`

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), forbidden) {
			t.Errorf("%s contains the forbidden status-key literal %s: the binder is a pure creator and must not touch main-owned status.captureState.commonController (decision #10)", name, forbidden)
		}
	}
}
