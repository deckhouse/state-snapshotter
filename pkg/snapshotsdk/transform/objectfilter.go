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

package transform

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// FilterSingleObject narrows compiled node output to the single object addressed by the object filter
// (opts.FilterKind/FilterName, optionally FilterAPIVersion). It runs AFTER compilation, on the already
// sanitized/namespace-rewritten objects, so an object dropped by the restore-safe sanitizer is simply
// absent (a 404, not restorable). subject names the addressed node ("<namespace>/<name>") for the
// not-found message. Outcomes: exactly one match -> that object; zero matches -> ErrNotFound (404); more
// than one match -> ErrBadRequest (400, fail-closed) — the contract guarantees a single object, so an
// ambiguous filter is never served.
func FilterSingleObject(objects []unstructured.Unstructured, opts Options, subject string) ([]unstructured.Unstructured, error) {
	matches := make([]unstructured.Unstructured, 0, 1)
	for i := range objects {
		if objects[i].GetKind() != opts.FilterKind || objects[i].GetName() != opts.FilterName {
			continue
		}
		if opts.FilterAPIVersion != "" && !apiVersionMatches(objects[i].GetAPIVersion(), opts.FilterAPIVersion) {
			continue
		}
		matches = append(matches, objects[i])
	}

	switch len(matches) {
	case 1:
		return matches[:1:1], nil
	case 0:
		return nil, fmt.Errorf("%w: object %s not found in node manifests of %s", ErrNotFound, filterIdentity(opts), subject)
	default:
		return nil, ambiguousFilterError(matches, opts)
	}
}

// apiVersionMatches reports whether an object's apiVersion satisfies the FilterAPIVersion, which may be a
// full "group/version" (exact match, e.g. "apps/v1" or the core "v1") or a bare "group" (matches any
// version in that group, e.g. "apps" matches "apps/v1").
func apiVersionMatches(objAPIVersion, filterAPIVersion string) bool {
	if objAPIVersion == filterAPIVersion {
		return true
	}
	gv, err := schema.ParseGroupVersion(objAPIVersion)
	if err != nil {
		return false
	}
	return gv.Group != "" && gv.Group == filterAPIVersion
}

// filterIdentity renders the requested object identity for the not-found message: "<apiVersion>/<Kind>/
// <name>" when an apiVersion was given, otherwise "<Kind>/<name>".
func filterIdentity(opts Options) string {
	if opts.FilterAPIVersion != "" {
		return fmt.Sprintf("%s/%s/%s", opts.FilterAPIVersion, opts.FilterKind, opts.FilterName)
	}
	return fmt.Sprintf("%s/%s", opts.FilterKind, opts.FilterName)
}

// ambiguousFilterError builds the fail-closed 400 for a kind+name filter that matched more than one
// object. Without an apiVersion it lists the competing apiVersions and asks the caller to disambiguate;
// with an apiVersion still ambiguous, it reports that the filter cannot narrow to a single object.
func ambiguousFilterError(matches []unstructured.Unstructured, opts Options) error {
	seen := make(map[string]struct{}, len(matches))
	apiVersions := make([]string, 0, len(matches))
	for i := range matches {
		av := matches[i].GetAPIVersion()
		if _, ok := seen[av]; ok {
			continue
		}
		seen[av] = struct{}{}
		apiVersions = append(apiVersions, av)
	}
	sort.Strings(apiVersions)
	if opts.FilterAPIVersion == "" {
		return fmt.Errorf(
			"%w: object filter kind=%s name=%s is ambiguous: it matches %d objects across apiVersions [%s]; specify apiVersion to disambiguate",
			ErrBadRequest, opts.FilterKind, opts.FilterName, len(matches), strings.Join(apiVersions, ", "),
		)
	}
	return fmt.Errorf(
		"%w: object filter kind=%s name=%s apiVersion=%s still matches %d objects [%s]; cannot narrow to a single object",
		ErrBadRequest, opts.FilterKind, opts.FilterName, opts.FilterAPIVersion, len(matches), strings.Join(apiVersions, ", "),
	)
}
