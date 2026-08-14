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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestCheckpointContentChunkRBACIsInternalOnly(t *testing.T) {
	repoRoot := filepath.Clean("../../../../..")
	templatesDir := filepath.Join(repoRoot, "templates")
	chunkResource := "manifestcheckpointcontentchunks"

	controllerTemplate := filepath.Join(templatesDir, "controller", "rbac-for-us.yaml")
	controllerRBAC := readTemplate(t, controllerTemplate)
	if strings.Count(controllerRBAC, chunkResource) != 1 {
		t.Fatalf("expected controller RBAC to mention %s exactly once", chunkResource)
	}
	if !strings.Contains(controllerRBAC, `resources: ["manifestcheckpointcontentchunks"]`) ||
		!strings.Contains(controllerRBAC, `verbs: ["create", "get", "delete"]`) {
		t.Fatalf("expected controller chunks RBAC to be exactly create/get/delete by name")
	}
	if strings.Contains(controllerRBAC, `resources: ["manifestcheckpointcontentchunks"]`+"\n"+`  verbs: ["create", "get", "list", "watch", "delete"]`) {
		t.Fatal("controller chunks RBAC must not grant list/watch")
	}

	// The delete-guard ValidatingAdmissionPolicy legitimately NAMES the chunk resource in its
	// matchConstraints (it protects chunks from direct deletion). Naming a resource in an admission policy
	// is not "granting direct access" — it is the opposite (it restricts DELETE/UPDATE), so this admission
	// template is exempt from the internal-only RBAC scan.
	deleteGuardTemplate := filepath.Join(templatesDir, "delete-guard.yaml")

	for _, path := range templateYAMLFiles(t, templatesDir) {
		if path == controllerTemplate || path == deleteGuardTemplate {
			continue
		}
		content := readTemplate(t, path)
		if strings.Contains(content, chunkResource) {
			t.Fatalf("%s must not grant direct access to %s", path, chunkResource)
		}
	}
}

func TestAdminKubeconfigRBACIsManualReadPath(t *testing.T) {
	repoRoot := filepath.Clean("../../../../..")
	adminTemplate := filepath.Join(repoRoot, "templates", "rbac-for-us.yaml")
	content := readTemplate(t, adminTemplate)

	okBlock := extractYAMLRuleBlock(content, "objectkeepers")
	if okBlock == "" {
		t.Fatal("admin-kubeconfig must mention objectkeepers for diagnostics")
	}
	for _, forbidden := range []string{"- patch", "- update", "- delete", "- create"} {
		if strings.Contains(okBlock, forbidden) {
			t.Fatalf("admin-kubeconfig objectkeepers rule must not include %s (forced TTL uses demo-e2e temp RBAC)", forbidden)
		}
	}
	mcrBlock := extractYAMLRuleBlock(content, "manifestcapturerequests")
	if strings.Contains(mcrBlock, "- create") || strings.Contains(mcrBlock, "- patch") || strings.Contains(mcrBlock, "- delete") {
		t.Fatal("admin-kubeconfig MCR/MCP must be read-only (get/list/watch)")
	}
	if !strings.Contains(content, "snapshots/manifests") || !strings.Contains(content, "manifestcheckpoints/manifests") {
		t.Fatal("admin-kubeconfig must grant aggregated manifests subresource get")
	}
}

// extractYAMLRuleBlock returns every rules[] entry of the (possibly Helm-templated) RBAC manifest
// that lists the given resource under its resources:, with comment lines dropped. Raw substring
// search over the whole file is NOT usable here, for two reasons that already produced a missed
// defect each: resources are also named inside header comments (the first match landed there and
// the scanned "block" was comment text), and a verb line that follows a comment still belongs to
// the preceding rule for the YAML parser (an orphaned "- delete" survived behind a comment while
// this guard stayed green).
func extractYAMLRuleBlock(content, resource string) string {
	var blocks []string
	var current []string
	flush := func() {
		if len(current) > 0 && ruleGrantsResource(current, resource) {
			blocks = append(blocks, strings.Join(current, "\n"))
		}
		current = nil
	}
	inRule := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // a comment never carries a grant and never terminates the rule around it
		}
		switch {
		case strings.HasPrefix(line, "- apiGroups:"):
			flush()
			inRule = true
			current = append(current, line)
		case inRule && (trimmed == "" || strings.HasPrefix(line, "  ")):
			current = append(current, line)
		default: // ---, apiVersion:, roleRef:, ... — the rules list is over
			flush()
			inRule = false
		}
	}
	flush()
	return strings.Join(blocks, "\n")
}

// ruleGrantsResource reports whether the rule (given as its comment-free lines) names the resource
// under its resources: key. Exact item match, so "snapshots" does not match "snapshots/status".
func ruleGrantsResource(lines []string, resource string) bool {
	inResources := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "resources:":
			inResources = true
		case strings.HasSuffix(trimmed, ":"):
			inResources = false
		case inResources && trimmed == "- "+resource:
			return true
		}
	}
	return false
}

// The two fixtures below pin the extractor against the exact failure modes that let a live defect
// through: a verb smuggled in after a comment must stay inside the rule, and a resource named only
// in a comment must not produce a block at all.

func TestExtractYAMLRuleBlockKeepsVerbAfterComment(t *testing.T) {
	const planted = `rules:
- apiGroups:
  - deckhouse.io
  resources:
  - objectkeepers
  verbs:
  - get
  - list
  - watch
# a comment used to terminate the scanned block right here
  - delete
- apiGroups:
  - other.io
  resources:
  - others
  verbs:
  - get
`
	block := extractYAMLRuleBlock(planted, "objectkeepers")
	if block == "" {
		t.Fatal("expected the objectkeepers rule to be extracted")
	}
	if !strings.Contains(block, "- delete") {
		t.Fatal("a verb following a comment belongs to the rule and must be visible to the guard")
	}
	if strings.Contains(block, "others") {
		t.Fatal("the neighboring rule must not leak into the extracted block")
	}
}

func TestExtractYAMLRuleBlockIgnoresCommentMentions(t *testing.T) {
	const fixture = `# Do NOT grant objectkeepers patch/update (comment only).
- apiGroups:
  - deckhouse.io
  resources:
  - objectkeepers
  verbs:
  - get
`
	block := extractYAMLRuleBlock(fixture, "objectkeepers")
	if strings.Contains(block, "patch") || strings.Contains(block, "update") {
		t.Fatal("comment text must not leak into the extracted rule block")
	}
	if !strings.Contains(block, "- get") {
		t.Fatal("the real rule body must be extracted")
	}
	if got := extractYAMLRuleBlock("# only a comment names objectkeepers\n", "objectkeepers"); got != "" {
		t.Fatalf("a comment-only mention must not produce a block, got %q", got)
	}
}

// TestCoreRBACDoesNotGrantDemoDomainResources enforces rbac-source-of-truth: no static RBAC template may
// hardcode a domain's resource names. Domain rights are granted dynamically by the 030-domain-rbac hook from
// the CSD-registered GVRs and signalled via CSD AccessGranted=True:
//
//   - controller SA        -> d8:state-snapshotter:controller:domain-read
//   - webhooks SA          -> d8:state-snapshotter:webhooks:domain-read (get on source GVRs, for MCR
//     target validation — a static allowlist here would only ever cover the domains someone remembered)
//   - DataExport SA        -> d8:state-snapshotter:data-export:domain-read
//
// The admin-kubeconfig template likewise does not enumerate domain groups: a domain module grants access to
// its own CRs and aggregated subresources in its own templates.
//
// delete-guard.yaml is excluded on purpose: it matches whole domain API *groups* (not resource names) so the
// admission guard stays kind-agnostic, which is the opposite of hardcoding an inventory.
func TestCoreRBACDoesNotGrantDemoDomainResources(t *testing.T) {
	repoRoot := filepath.Clean("../../../../..")
	templates := []string{
		filepath.Join(repoRoot, "templates", "controller", "rbac-for-us.yaml"),
		filepath.Join(repoRoot, "templates", "webhooks", "rbac-for-us.yaml"),
		filepath.Join(repoRoot, "templates", "rbac-for-us.yaml"),
	}

	for _, tmpl := range templates {
		content := readTemplate(t, tmpl)
		for _, forbidden := range []string{
			"sds-unified-snapshots-poc.deckhouse.io",
			"demovirtualmachines",
			"demovirtualdisks",
		} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("%s must not hardcode demo/domain RBAC resource %q (grant it dynamically via the 030-domain-rbac hook / AccessGranted)", tmpl, forbidden)
			}
		}
	}
}

func TestWebhookRBACDoesNotUseWildcardResourceReads(t *testing.T) {
	repoRoot := filepath.Clean("../../../../..")
	webhookTemplate := filepath.Join(repoRoot, "templates", "webhooks", "rbac-for-us.yaml")
	content := readTemplate(t, webhookTemplate)

	for _, forbidden := range []string{
		"apiGroups:\n      - \"*\"",
		"resources:\n      - \"*\"",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("webhook RBAC must not use wildcard read rule %q", forbidden)
		}
	}
}

func templateYAMLFiles(t *testing.T, root string) []string {
	t.Helper()

	var files []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk templates dir %s: %v", root, err)
	}
	return files
}

func readTemplate(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
