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

func extractYAMLRuleBlock(content, resource string) string {
	idx := strings.Index(content, resource)
	if idx < 0 {
		return ""
	}
	// Walk back to apiGroups and forward until next apiGroups or end of rules.
	start := strings.LastIndex(content[:idx], "apiGroups:")
	if start < 0 {
		start = idx
	}
	end := strings.Index(content[idx:], "\n- apiGroups:")
	if end < 0 {
		return content[start:]
	}
	return content[start : idx+end]
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
