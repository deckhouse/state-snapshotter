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

package transition

import "testing"

func TestModuleTagPattern(t *testing.T) {
	accepted := []string{"mr153", "pr78", "main", "mr1", "pr9999"}
	for _, v := range accepted {
		if !moduleTagPattern.MatchString(v) {
			t.Errorf("%q should be accepted by moduleTagPattern (mr<N>/pr<N>/main)", v)
		}
	}

	rejected := []string{
		"\u0430\u0435\u0442", // Cyrillic U+0430 U+0435 U+0442 (a tag entered in a non-Latin keyboard layout instead of "main") — the bug this guards against
		"v0.1.25",            // prod tag: not in the dev registry the nested cluster pulls
		"v0.2.0",             // prod tag
		"MR153",              // wrong case
		"mr",                 // no number
		"pr",                 // no number
		"mr153-rc1",          // suffix
		"feature-x",          // arbitrary branch
		"",                   // empty
	}
	for _, v := range rejected {
		if moduleTagPattern.MatchString(v) {
			t.Errorf("%q should be rejected by moduleTagPattern", v)
		}
	}
}

func TestIsASCII(t *testing.T) {
	if !isASCII("main") || !isASCII("mr153") {
		t.Error("plain Latin tags must be ASCII")
	}
	// "\u0430\u0435\u0442" is Cyrillic (multi-byte UTF-8) — the exact wrong-keyboard-layout value that wedged a run.
	if isASCII("\u0430\u0435\u0442") {
		t.Error("Cyrillic runes must be flagged as non-ASCII")
	}
}
