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

package namespacemanifest

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestManifestCaptureRequestObjectKeeperName(t *testing.T) {
	t.Run("same namespace name and UID produce same name", func(t *testing.T) {
		first := ManifestCaptureRequestObjectKeeperName("ns", "mcr", types.UID("uid-1"))
		second := ManifestCaptureRequestObjectKeeperName("ns", "mcr", types.UID("uid-1"))
		if first != second {
			t.Fatalf("expected stable name for same MCR UID, got %q and %q", first, second)
		}
	})

	t.Run("same namespace and name with different UID produce different names", func(t *testing.T) {
		first := ManifestCaptureRequestObjectKeeperName("ns", "mcr", types.UID("uid-1"))
		second := ManifestCaptureRequestObjectKeeperName("ns", "mcr", types.UID("uid-2"))
		if first == second {
			t.Fatalf("expected different names for different MCR UIDs, got %q", first)
		}
	})

	t.Run("long namespace and name produce valid DNS-1123 name", func(t *testing.T) {
		got := ManifestCaptureRequestObjectKeeperName(
			"very-long-namespace-name-that-would-not-fit-in-a-retainer-name",
			"very-long-manifest-capture-request-name-that-would-not-fit",
			types.UID("uid-with-dashes-and-long-enough-to-exercise-hashing"),
		)
		if len(got) > 63 {
			t.Fatalf("expected name length <= 63, got %d for %q", len(got), got)
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Fatalf("expected DNS-1123 subdomain name, got %q: %v", got, errs)
		}
	})
}
