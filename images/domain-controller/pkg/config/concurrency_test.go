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

package config

import "testing"

func TestResolveMaxConcurrentReconciles(t *testing.T) {
	const def = 4
	for _, tc := range []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"unset keeps default", "", def, false},
		{"whitespace keeps default", "  ", def, false},
		{"valid override", "8", 8, false},
		{"valid override trimmed", " 16 ", 16, false},
		{"zero invalid", "0", 0, true},
		{"negative invalid", "-1", 0, true},
		{"non-numeric invalid", "four", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMaxConcurrentReconciles("TEST_ENV", tc.raw, def)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got n=%d", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("resolveMaxConcurrentReconciles(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
