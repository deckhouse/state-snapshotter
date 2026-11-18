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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/api"
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

// volumeNodeSubresourceVerbs is the verb each volume-node endpoint is authorized under: the aggregated
// API server builds a SubjectAccessReview from the URL, so a GET reads as "get" and a POST as "create".
var volumeNodeSubresourceVerbs = map[string]string{
	"volumesnapshots/manifests-download":                 "get",
	"volumesnapshots/manifests-with-data-restoration":    "get",
	"volumesnapshots/manifests-and-children-refs-upload": "create",
}

const (
	// volumeNodeSubresourceGroup is the aggregated group the volume node's endpoints are served under. It
	// is named after the module that OWNS the CSI VolumeSnapshot, not after this one, and that is the
	// whole point of naming it here: nothing in this repository serves it, so nothing here would notice
	// it being dropped from a role.
	volumeNodeSubresourceGroup = "subresources.storage-foundation.deckhouse.io"

	// deprecatedVolumeNodeSubresourceGroup is the group those endpoints were served under before the move.
	// It is answered by a compatibility alias for clients released on another schedule, and it must stay
	// granted for as long as the alias exists: authorization is decided from the group in the URL, so a
	// request on the old path would be refused 403 while reaching a working endpoint. It is dropped
	// together with the alias, in one change.
	deprecatedVolumeNodeSubresourceGroup = "subresources.snapshot.storage.k8s.io"
)

// TestAdminKubeconfigGrantsVolumeNodeEndpointsUnderBothGroups pins the volume-node half of the
// admin-kubeconfig role: the manual-kubectl and demo path walks trees that contain volume nodes, and both
// the current group and the deprecated alias must be reachable through it — the alias because the
// walkthrough that checks the alias works uses this very kubeconfig, and a 403 there is indistinguishable
// from a broken alias.
//
// Only the volume-node grants are pinned, in both directions: every expected (group, subresource, verb)
// is present, and no OTHER group grants a volumesnapshots subresource. The rest of the role is loosely
// asserted by TestAdminKubeconfigRBACIsManualReadPath — pinning it whole here would duplicate that.
func TestAdminKubeconfigGrantsVolumeNodeEndpointsUnderBothGroups(t *testing.T) {
	repoRoot := filepath.Clean("../../../../..")
	adminTemplate := filepath.Join(repoRoot, "templates", "rbac-for-us.yaml")
	grants := flattenPolicyRules(policyRulesOfHelmClusterRole(t, adminTemplate, "d8:state-snapshotter:admin-kubeconfig"))

	checked := 0
	for _, group := range []string{volumeNodeSubresourceGroup, deprecatedVolumeNodeSubresourceGroup} {
		for subresource, verb := range volumeNodeSubresourceVerbs {
			checked++
			key := group + "/" + subresource
			verbs, granted := grants[key]
			switch {
			case !granted:
				t.Errorf("admin-kubeconfig grants nothing on %q — a tree walk that reaches a volume node answers 403 there (granted: %s)",
					key, strings.Join(sortedGrantLines(grants), "; "))
			case !reflect.DeepEqual(verbs, []string{verb}):
				t.Errorf("admin-kubeconfig grants %v on %q, want exactly [%s]", verbs, key, verb)
			}
		}
	}

	for key := range grants {
		group, resource, found := strings.Cut(key, "/")
		if !found || !strings.HasPrefix(resource, "volumesnapshots/") {
			continue
		}
		if group != volumeNodeSubresourceGroup && group != deprecatedVolumeNodeSubresourceGroup {
			t.Errorf("admin-kubeconfig grants %q on %q — a volume node's endpoints are served under %q, and %q is neither that nor the deprecated alias this role keeps",
				grants[key], key, volumeNodeSubresourceGroup, group)
		}
	}

	t.Logf("checked %d volume-node grant(s) across 2 group(s) in %s", checked, adminTemplate)
	if checked == 0 {
		t.Fatal("no volume-node grant checked — zero findings means the expectation table emptied out, not that the role is right")
	}
}

// TestControllerRBACDoesNotNameTheVolumeNodeOwnersGroup pins the boundary that makes the delegation of a
// volume node work: the right to call the OWNER's restore subresource is granted to this controller's
// ServiceAccount by the owner's own chart, never by a static template of this one. A module knows its own
// group name; this one does not, and deriving it mechanically from the CR's group gives a group nobody
// serves.
//
// SCOPE — exactly one file, the controller's own template, and this narrowness is the point rather than
// laziness: templates/rbac-for-us.yaml (the manual-kubectl role) NEEDS that group and is granted it
// deliberately, so adding this string to the forbidden list of
// TestCoreRBACDoesNotGrantDemoDomainResources — which scans that file too — would make it permanently
// red and invite "fixing" it by deleting a grant that has to be there.
//
// LIMIT: it reads one template as text. It says nothing about what the owner's chart grants (another
// repository) and nothing about the other templates of this one.
func TestControllerRBACDoesNotNameTheVolumeNodeOwnersGroup(t *testing.T) {
	repoRoot := filepath.Clean("../../../../..")
	controllerTemplate := filepath.Join(repoRoot, "templates", "controller", "rbac-for-us.yaml")
	content := readTemplate(t, controllerTemplate)

	if strings.Contains(content, volumeNodeSubresourceGroup) {
		t.Errorf("%s names %q — the controller's access to another module's aggregated subresource is granted by THAT module's chart, and a static grant here is the copy that goes stale when the group is renamed",
			controllerTemplate, volumeNodeSubresourceGroup)
	}

	// Read as text, so a spelling this comparison would miss is worth knowing about: the scan is only
	// meaningful while the file it reads is the controller's RBAC.
	if !strings.Contains(content, "d8:{{ .Chart.Name }}:controller") {
		t.Fatalf("%s no longer looks like the controller's RBAC template — the scan above then proves nothing", controllerTemplate)
	}
	t.Logf("scanned %s (%d bytes) for the volume node owner's group %q", controllerTemplate, len(content), volumeNodeSubresourceGroup)
}

// policyRulesOfHelmClusterRole parses the rules of one ClusterRole out of a Helm template. The Helm
// actions are dropped line by line first: the templates in this repository use them only for labels and
// the namespace, so what is left is the YAML this comparison needs, and the alternative — substring
// matching over the raw text — is what let a live defect through before (see extractYAMLRuleBlock).
//
// The role is looked up by name, and a miss is fatal: a renamed role would otherwise silently reduce the
// caller to comparing an empty rule set.
func policyRulesOfHelmClusterRole(t *testing.T, path, roleName string) []rbacv1.PolicyRule {
	t.Helper()

	var kept []string
	for _, line := range strings.Split(readTemplate(t, path), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{{") {
			continue
		}
		kept = append(kept, line)
	}

	for _, doc := range strings.Split(strings.Join(kept, "\n"), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if role.Kind == "ClusterRole" && role.Name == roleName {
			if len(role.Rules) == 0 {
				t.Fatalf("%s: ClusterRole %q parsed with no rules — the Helm-action stripping above broke the document", path, roleName)
			}
			return role.Rules
		}
	}
	t.Fatalf("%s holds no ClusterRole named %q", path, roleName)
	return nil
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

// ---------------------------------------------------------------------------
// User capability roles and the roles that collect them —
// templates/rbacv2/use/*.yaml, templates/rbacv2/use/roles/*.yaml,
// templates/backup-agent/*.yaml and templates/rbacv2-compat/*.yaml
//
// Plain testing guards reading the chart from disk rather than Ginkgo specs over embedded fixtures:
// the subject of every check below IS the template file the chart ships, so an embedded copy would be
// a copy checking itself and a passing test would say nothing about what the cluster gets.
//
// TWO ROLE MODELS are shipped from one chart. DKP 1.78 replaced the manage/use kinds of RBACv2 with
// capability/scope, and this module is installed on clusters on both sides of that release, so every
// template here carries the object of each model behind a gate on global.deckhouseVersion (see
// templates/_rbacv2_compat.tpl). Both branches reach a cluster, so both are pinned: the new one in full,
// the legacy one by name, labels and grants, and the two against each other where they must agree.
// ---------------------------------------------------------------------------

// rbacScheme names the branch of a gated template the guards read: the capability/scope model of DKP
// 1.78 and later, or the legacy manage/use model rendered below it.
type rbacScheme string

const (
	schemeNew    rbacScheme = "capability/scope (DKP >= 1.78)"
	schemeLegacy rbacScheme = "manage/use (DKP < 1.78)"
)

var rbacSchemes = []rbacScheme{schemeNew, schemeLegacy}

// The gate lines exactly as rbacv2-migrate-module.sh writes them (the "eq" shape, optionally with an
// else branch) and as the legacy-only file of the collecting role spells its own ("ne"). Matched as
// whole lines, so a gate on anything else is not mistaken for this one.
const (
	rbacGateNewScheme    = `{{- if eq (include "state-snapshotter.rbacv2_new_scheme" .) "true" }}`
	rbacGateLegacyScheme = `{{- if ne (include "state-snapshotter.rbacv2_new_scheme" .) "true" }}`
	rbacGateElse         = `{{- else }}`
	rbacGateEnd          = `{{- end }}`
)

const (
	// capabilityRolePrefix is the name every user capability role of this module carries in the new
	// model; the file base name is the capability that follows it.
	capabilityRolePrefix = "d8:namespace-capability:state-snapshotter:"

	// legacyCapabilityRolePrefix is the name the same capability carries in the legacy branch.
	legacyCapabilityRolePrefix = "d8:use:capability:module:state-snapshotter:"

	// capabilityMarkerPrefix is the value prefix of rbac.deckhouse.io/capability, the marker the
	// platform keeps unique across the cluster; the capability follows it.
	capabilityMarkerPrefix = "namespace-capability.state-snapshotter."

	// namespaceLevelLabel is the level a new-model capability aggregates into. That lineage is granted
	// per namespace, through a RoleBinding, so it is not the cluster-wide road the legacy axis below
	// is; the level it carries is pinned per role all the same, because it decides who in a namespace
	// gains the capability without anyone binding it separately.
	namespaceLevelLabel = "rbac.deckhouse.io/aggregate-to-namespace-as"

	// legacyAggregationLabel is the cluster-level axis of the legacy model. A role carrying it is
	// handed to everyone already holding the matching cluster-wide role, with nobody binding anything,
	// so its presence is part of the contract and is pinned per role below.
	legacyAggregationLabel = "rbac.deckhouse.io/aggregate-to-kubernetes-as"

	// backupAgentMemberLabel composes, in the new model, the capabilities an external backup product
	// needs into one role for it to be bound to. It is a label of this module, outside
	// rbac.deckhouse.io/, because the platform contract refuses an axis it does not know there. It
	// is not a road onto any level: the role collecting it carries nothing of the role model and
	// grants nothing at all until a RoleBinding names it.
	backupAgentMemberLabel = "state-snapshotter.deckhouse.io/aggregate-to-backup-agent"

	// legacyBackupAgentLabel is the same membership in the legacy model, spelled as an axis of the
	// platform's own prefix. That is why a legacy capability may carry this axis while deliberately
	// refusing legacyAggregationLabel: the difference between them is the difference between being
	// bound explicitly and being handed out to a whole level. Written down because the flat ban this
	// replaced looked right, and restoring it would take the single-binding path away.
	legacyBackupAgentLabel = "rbac.deckhouse.io/aggregate-to-backup-agent-as"

	// rbacAggregationAxisPrefix is what every aggregation axis of this platform is named with. The
	// guards below compare against the PREFIX and not against a list of axis names, because the list
	// is not this repository's to know: the platform ships axes of its own, and a role of this module
	// carrying one of them is composed into a set assembled elsewhere, on terms nothing here states.
	rbacAggregationAxisPrefix = "rbac.deckhouse.io/aggregate-"

	// backupAgentRoleName is the role that collects backupAgentMemberLabel. It is an ordinary
	// ClusterRole outside the role model — the 1.78 model has no role of a module — and lives in
	// templates/backup-agent, away from the capabilities, because a role with an aggregationRule among
	// them is refused below, for a reason that still holds.
	backupAgentRoleName = "d8:state-snapshotter:backup-agent"

	// legacyBackupAgentRoleName is the collecting role of the legacy model, shipped from
	// templates/rbacv2/use/roles below 1.78 and kept from 1.78 on as a deprecated alias in
	// templates/rbacv2-compat, so the bindings made before the upgrade keep granting.
	legacyBackupAgentRoleName = legacyCapabilityRolePrefix + "backup_agent"

	// backupAgentSystemRoleName is the cluster-scoped counterpart of backupAgentRoleName, bound with a
	// ClusterRoleBinding. It lives in templates/backup-agent-system and is the same object in both models.
	backupAgentSystemRoleName = "d8:state-snapshotter:backup-agent-system"
)

// capabilityI18nAnnotations are the annotations the console shows next to a capability of the new
// model, and the only annotations such a capability may carry. Every one is required: the platform
// contract refuses a capability missing any of them.
var capabilityI18nAnnotations = []string{
	"en.meta.deckhouse.io/title",
	"ru.meta.deckhouse.io/title",
	"en.meta.deckhouse.io/description",
	"ru.meta.deckhouse.io/description",
}

// TestUserCapabilityRolesGrantExactRulesAndLabels pins every user capability role by name, in both
// branches of its template: the exact set of "<apiGroup>/<resource>" grants with their verbs, the level
// it aggregates into, its membership in the backup-agent set, and that nothing else composes the role
// into somebody's rights. Rules are compared flattened, so regrouping the YAML is free while adding or
// widening a grant is not — and the two branches are held to the SAME grants, so a fix applied to one
// model only fails here instead of reaching half the clusters.
//
// NEW MODEL: the labels are compared as an exact set. Every label a capability of the new model may
// carry is part of the contract — the kind, the scope, the unique marker, one level, the module label
// for the backup-agent set — so anything beyond that set is a way in nothing here describes, and
// anything missing is a capability the platform refuses or a collector that stays short of it. The
// annotations are the four texts the console shows and nothing else.
//
// LEGACY MODEL: two axes, and the difference between them is why one is allowed where the other is
// refused: legacyAggregationLabel puts the capability into a cluster-wide access level, so every holder
// of that level gains it with nobody binding anything; legacyBackupAgentLabel puts it into a role that
// carries no axis of its own and grants nothing until a RoleBinding names it. Both are pinned per role
// below, in both directions.
//
// LIMIT: this compares what the role SAYS. That the granted subresources are the ones the server
// actually serves is a different question, checked by TestEveryNamespacedSubresourceRouteHasCapabilityRole.
func TestUserCapabilityRolesGrantExactRulesAndLabels(t *testing.T) {
	expected := map[string]struct {
		namespaceAs  string // the level of the new model; every capability aggregates into one
		kubernetesAs string // "" — the legacy object deliberately stays off the cluster-level axis
		backupAgent  bool   // part of the set the backup-agent role collects, in both models
		grants       map[string][]string
	}{
		"view_snapshots": {
			namespaceAs:  "viewer",
			kubernetesAs: "viewer",
			backupAgent:  true,
			grants: map[string][]string{
				"state-snapshotter.deckhouse.io/snapshots":        {"get", "list", "watch"},
				"state-snapshotter.deckhouse.io/snapshots/status": {"get", "list", "watch"},
				// A tree's volume nodes are addressed as VolumeSnapshots, and their status is where a
				// walk reads the bound content from — without it the tree cannot be traversed.
				"snapshot.storage.k8s.io/volumesnapshots": {"get", "list", "watch"},
			},
		},
		"manage_snapshots": {
			namespaceAs:  "manager",
			kubernetesAs: "manager",
			backupAgent:  true,
			// No update/patch: a Snapshot spec is one-shot and immutable.
			grants: map[string][]string{
				"state-snapshotter.deckhouse.io/snapshots": {"create", "delete"},
			},
		},
		"download_snapshots": {
			namespaceAs:  "admin",
			kubernetesAs: "",
			backupAgent:  true,
			grants: map[string][]string{
				"subresources.state-snapshotter.deckhouse.io/snapshots/manifests-download":        {"get"},
				"subresources.storage-foundation.deckhouse.io/volumesnapshots/manifests-download": {"get"},
				// The group the volume node's endpoints were served from before they moved. It is still
				// granted because a SubjectAccessReview is built from the group in the URL: a client
				// released on another schedule addresses the old path, the server answers it through a
				// compatibility alias, and without this grant the answer would be 403 instead. Both
				// entries go away together with that alias, in one change — until then a MISSING one
				// here is the defect, not an extra one.
				"subresources.snapshot.storage.k8s.io/volumesnapshots/manifests-download": {"get"},
			},
		},
		"upload_snapshots": {
			// Namespace admin in the new model, off the cluster-level axis in the legacy one, for one
			// reason: this capability writes into ANOTHER module's resource, so a cluster-wide road
			// would let every manager in the cluster create VolumeSnapshots merely because this module
			// is installed. The namespace lineage is granted per namespace, and admin — the level that
			// runs the namespace and can already reach a volume's contents through the pod mounting it
			// — gains nothing new in kind. In the backup-agent set all the same: loading a tree back
			// into the cluster is half of what a backup product does, and that set reaches an
			// explicitly bound subject only.
			namespaceAs:  "admin",
			kubernetesAs: "",
			backupAgent:  true,
			grants: map[string][]string{
				"subresources.state-snapshotter.deckhouse.io/snapshots/manifests-and-children-refs-upload":        {"create"},
				"subresources.storage-foundation.deckhouse.io/volumesnapshots/manifests-and-children-refs-upload": {"create"},
				// The deprecated group of the volume node's endpoints; see download_snapshots above.
				"subresources.snapshot.storage.k8s.io/volumesnapshots/manifests-and-children-refs-upload": {"create"},
				// The objects standing for volume nodes of an imported tree are created by the client,
				// not by the controller, so an import of a tree with a volume in it needs create here.
				// Create only: deleting a VolumeSnapshot destroys the snapshot data behind it, and
				// undoing an import is deleting the root Snapshot the nodes are owned by, which is
				// manage_snapshots' business.
				"snapshot.storage.k8s.io/volumesnapshots": {"create"},
			},
		},
		"restore_snapshots": {
			// Namespace admin in the new model, off the cluster-level axis in the legacy one: this
			// capability reads a whole subtree already rewritten for a target namespace, which is not
			// something a holder of a general cluster-wide role is assumed to have asked for. A
			// namespace admin can already reach a volume's contents, so it gains nothing new in kind.
			// In the backup-agent set, because restoring is the other half of what a backup product
			// does, and membership there grants nothing until somebody binds the collecting role in a
			// namespace on purpose.
			namespaceAs:  "admin",
			kubernetesAs: "",
			backupAgent:  true,
			grants: map[string][]string{
				"subresources.state-snapshotter.deckhouse.io/snapshots/manifests-with-data-restoration":        {"get"},
				"subresources.storage-foundation.deckhouse.io/volumesnapshots/manifests-with-data-restoration": {"get"},
				// The deprecated group of the volume node's endpoints; see download_snapshots above.
				"subresources.snapshot.storage.k8s.io/volumesnapshots/manifests-with-data-restoration": {"get"},
			},
		},
	}

	files := capabilityRoleFiles(t)
	if len(files) != len(expected) {
		t.Fatalf("templates/rbacv2/use holds %d capability file(s) but %d capability role(s) are pinned here: %v",
			len(files), len(expected), files)
	}

	grantsChecked := 0
	for _, file := range files {
		capability := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		want, pinned := expected[capability]
		if !pinned {
			t.Errorf("%s: capability %q is published with no expectation here — every capability this module hands out is pinned by name, so add its grants and aggregation labels above", file, capability)
			continue
		}
		if want.namespaceAs == "" {
			t.Errorf("%s: capability %q is pinned with no level of the new model — the platform contract refuses a capability that aggregates into no level, so pin the one it carries", file, capability)
		}

		roles := map[rbacScheme]rbacv1.ClusterRole{}
		for _, scheme := range rbacSchemes {
			parsed := clusterRolesFromTemplate(t, file, scheme)
			if len(parsed) != 1 {
				t.Errorf("%s: the %s branch holds %d ClusterRole(s), want exactly one — both models ship this capability", file, scheme, len(parsed))
				continue
			}
			roles[scheme] = parsed[0]
		}
		if len(roles) != len(rbacSchemes) {
			continue
		}

		checkNewCapabilityRole(t, file, capability, roles[schemeNew], want.namespaceAs, want.backupAgent)
		checkLegacyCapabilityRole(t, file, capability, roles[schemeLegacy], want.kubernetesAs, want.backupAgent)

		for _, scheme := range rbacSchemes {
			role := roles[scheme]
			// An aggregationRule on the capability itself makes the controller overwrite its rules
			// from whatever matches the selector — after which the comparison below describes the file
			// and not the role the cluster ends up with. That ban is why the roles collecting the
			// backup-agent set live outside this directory instead of among the capabilities.
			if role.AggregationRule != nil {
				t.Errorf("%s (%s): declares an aggregationRule — its rules would then be rewritten by the controller from the roles the selector matches, and everything pinned below would stop describing the effective role", file, scheme)
			}
			for _, rule := range role.Rules {
				if len(rule.ResourceNames) > 0 || len(rule.NonResourceURLs) > 0 {
					t.Errorf("%s (%s): a rule narrows to resourceNames/nonResourceURLs %v/%v, which the flattened comparison below does not model — pin it explicitly or drop it",
						file, scheme, rule.ResourceNames, rule.NonResourceURLs)
				}
			}
			if grants := flattenPolicyRules(role.Rules); !reflect.DeepEqual(grants, want.grants) {
				t.Errorf("%s (%s): grants\n  %v\nwant\n  %v", file, scheme, sortedGrantLines(grants), sortedGrantLines(want.grants))
			}
			grantsChecked += len(want.grants)
		}
	}

	t.Logf("checked %d capability role file(s) in %d model(s) and %d grant(s) under templates/rbacv2/use", len(files), len(rbacSchemes), grantsChecked)
	if grantsChecked == 0 {
		t.Fatal("no grant checked — zero findings means the roles stopped being read, not that they are correct")
	}
}

// checkNewCapabilityRole pins the new-model object of one capability: its name, its labels as an exact
// set and its annotations as exactly the four console texts.
func checkNewCapabilityRole(t *testing.T, file, capability string, role rbacv1.ClusterRole, namespaceAs string, backupAgent bool) {
	t.Helper()

	if want := capabilityRolePrefix + capability; role.Name != want {
		t.Errorf("%s: the new-model role is named %q, want %q (the file base name is the capability)", file, role.Name, want)
	}

	// Exact, in both directions. A label outside this set is refused whatever it is called: a third
	// rbac.deckhouse.io/aggregate-* axis composes the capability into a set nothing here bounds, the
	// legacy axes mean nothing to a 1.78 cluster and are refused by its contract, and
	// rbac.deckhouse.io/delegatable would put a single capability into what a project owner hands out.
	// A MISSING label is as bad the other way: without the level the contract refuses the capability,
	// without the module label the backup-agent role silently collects one capability fewer.
	wantLabels := map[string]string{
		"heritage":                     "deckhouse",
		"module":                       "state-snapshotter",
		"rbac.deckhouse.io/kind":       "capability",
		"rbac.deckhouse.io/scope":      "namespace",
		"rbac.deckhouse.io/capability": capabilityMarkerPrefix + capability,
		namespaceLevelLabel:            namespaceAs,
	}
	if backupAgent {
		wantLabels[backupAgentMemberLabel] = "true"
	}
	if !reflect.DeepEqual(role.Labels, wantLabels) {
		t.Errorf("%s: the new-model role carries labels\n  %v\nwant exactly\n  %v\n— every label of a capability decides who gains it or whether the platform accepts it, so none is incidental",
			file, sortedLabelLines(role.Labels), sortedLabelLines(wantLabels))
	}
	if marker := capabilityMarkerPrefix + capability; len(marker) > 63 {
		t.Errorf("%s: the capability marker %q is %d characters long, and a label value holds 63 at most", file, marker, len(marker))
	}

	// The four texts the console shows, and no other annotation: the access levels of the cluster roles
	// this module ships are assembled from an annotation, so one access-level annotation on a
	// capability hands it to an entire level.
	for key, value := range role.Annotations {
		if !slices.Contains(capabilityI18nAnnotations, key) {
			t.Errorf("%s: the new-model role carries annotation %q=%q — a capability carries its four console texts and nothing else, and any other annotation is another way into somebody's rights", file, key, value)
		}
	}
	for _, key := range capabilityI18nAnnotations {
		if strings.TrimSpace(role.Annotations[key]) == "" {
			t.Errorf("%s: the new-model role has no %s — the platform contract refuses a capability without it, and the console shows it next to the capability", file, key)
		}
	}
	// The migration script writes these texts from the action's name and asks for them to be reworded;
	// what it writes is recognizable, and shipping it would show a placeholder in the console.
	enTitle := role.Annotations["en.meta.deckhouse.io/title"]
	if !strings.HasPrefix(enTitle, "Module state-snapshotter: ") {
		t.Errorf("%s: the English title %q does not start with \"Module state-snapshotter: \", the form every capability of the platform is titled in", file, enTitle)
	}
	if action := strings.TrimPrefix(enTitle, "Module state-snapshotter: "); action != "" && strings.Contains(role.Annotations["ru.meta.deckhouse.io/title"], action) {
		t.Errorf("%s: the Russian title repeats the English action %q — that is the placeholder of the migration script, not a translation", file, action)
	}
	if strings.HasSuffix(role.Annotations["en.meta.deckhouse.io/description"], " capability of the state-snapshotter module.") ||
		strings.HasPrefix(role.Annotations["ru.meta.deckhouse.io/description"], "Capability ") {
		t.Errorf("%s: a description is the placeholder of the migration script — say what the capability grants, in both languages", file)
	}
}

// checkLegacyCapabilityRole pins the legacy object of one capability the way it has been pinned since
// before the new model existed: name, the labels the legacy aggregates select on, both axes in both
// directions, and no annotation at all.
func checkLegacyCapabilityRole(t *testing.T, file, capability string, role rbacv1.ClusterRole, kubernetesAs string, backupAgent bool) {
	t.Helper()

	if want := legacyCapabilityRolePrefix + capability; role.Name != want {
		t.Errorf("%s: the legacy role is named %q, want %q (the file base name is the capability)", file, role.Name, want)
	}
	for key, value := range map[string]string{
		"heritage":               "deckhouse",
		"module":                 "state-snapshotter",
		"rbac.deckhouse.io/kind": "use",
	} {
		if got := role.Labels[key]; got != value {
			t.Errorf("%s: the legacy role's label %q is %q, want %q", file, key, got, value)
		}
	}
	// A legacy capability reaches somebody's rights by exactly two mechanisms: the two axes pinned
	// here. A THIRD rbac.deckhouse.io/aggregate-* axis, whose collecting role this repository does not
	// describe, and any annotation — the access levels of the cluster roles this module ships are
	// assembled from one — would do it quietly, so both are refused outright. So is the module label of
	// the new model: the legacy collector does not select on it, and it would only make the legacy
	// object look like a member it is not.
	for key := range role.Annotations {
		t.Errorf("%s: the legacy role carries annotation %q — a capability role is composed into somebody's rights through its aggregation label and nothing else, and an annotation is another way in", file, key)
	}
	for key := range role.Labels {
		if strings.HasPrefix(key, rbacAggregationAxisPrefix) &&
			key != legacyAggregationLabel && key != legacyBackupAgentLabel {
			t.Errorf("%s: the legacy role carries a third aggregation axis %q — the legacy capabilities of this module go onto %s and %s only, and an axis described nowhere in this repository composes them into rights nothing here bounds",
				file, key, legacyAggregationLabel, legacyBackupAgentLabel)
		}
	}
	if value, labelled := role.Labels[backupAgentMemberLabel]; labelled {
		t.Errorf("%s: the legacy role carries %s=%q, the membership label of the new model — the legacy collector selects on %s, so this label only misstates what the object is part of",
			file, backupAgentMemberLabel, value, legacyBackupAgentLabel)
	}

	got, labelled := role.Labels[legacyAggregationLabel]
	switch {
	case kubernetesAs == "" && labelled:
		t.Errorf("%s: the legacy role carries %s=%q, but this capability must stay off the cluster-level axis — the label would hand it to every holder of that cluster-wide role, silently", file, legacyAggregationLabel, got)
	case kubernetesAs != "" && got != kubernetesAs:
		t.Errorf("%s: the legacy role aggregates as %q, want %q", file, got, kubernetesAs)
	}

	gotAgent, agentLabelled := role.Labels[legacyBackupAgentLabel]
	switch {
	case !backupAgent && agentLabelled:
		t.Errorf("%s: the legacy role carries %s=%q, but this capability is not pinned as part of the backup-agent set — the label adds its grants to the one role an external backup product is bound to", file, legacyBackupAgentLabel, gotAgent)
	case backupAgent && gotAgent != "member":
		t.Errorf("%s: the legacy role is in the backup-agent set as %q, want %q — the selector of %s matches on the value, so anything else leaves this capability out of the single binding and its absence surfaces only when a client is refused",
			file, gotAgent, "member", legacyBackupAgentRoleName)
	}
}

// TestBackupAgentAggregateRoleCollectsOneAxisAndSpreadsThroughNone pins the role an external backup
// product is bound to, in each model: its name, the labels it carries, the ones it must not, what its
// rules are in the chart, and that its aggregationRule is exactly one selector over exactly two labels.
//
// NEW MODEL: d8:state-snapshotter:backup-agent in templates/backup-agent, an ordinary ClusterRole
// OUTSIDE the role model — the model of 1.78 has no role of a module. Its labels are pinned as an exact
// set of two, heritage and module: no rbac.deckhouse.io/kind, so none of the platform's own aggregates
// can ever select it, and no rbac.deckhouse.io/* label at all. It has no rules key, not even an empty
// one: rules belong to the aggregation controller, and a rules key in the chart makes server-side apply
// fight it, emptying the role for a moment on every reconcile.
//
// LEGACY MODEL: d8:use:capability:module:state-snapshotter:backup_agent in templates/rbacv2/use/roles,
// pinned as it always was, rules: [] included — that branch renders byte-for-byte what earlier releases
// shipped.
//
// WHAT IS ABSENT is the security condition of the whole arrangement in both models, so it is pinned by
// absence rather than described in a comment. Three ways in, and none of them is pinned by name — a
// named ban only forbids the axis somebody already thought of, and the platform ships axes this
// repository does not know:
//
//   - ANY label under rbacAggregationAxisPrefix, and the membership label of this module. Composed onto
//     an axis, this role hands the whole set — reading subtrees rewritten for another namespace, loading
//     trees back in, moving a volume's bytes — to every holder of the corresponding access level, with
//     nobody binding anything. The legacy role already carries rbac.deckhouse.io/kind: use, the other
//     half of what the platform's own legacy aggregates select on, so a single line naming any of their
//     axes is enough there.
//   - ANY annotation, refused exactly as it is on the legacy capabilities this role collects: the
//     access levels of the cluster roles shipped here are assembled from an annotation, and a hook turns
//     one on a role into bindings for the subjects of that level — including a CLUSTER-wide one, which
//     is precisely the boundary "grants nothing until a RoleBinding names it" claims to hold.
//   - rbac.deckhouse.io/delegatable, named separately because it is not under that prefix. It would add
//     this role to the set a project owner may hand out inside their own project, while connecting an
//     external backup product to a cluster is not a per-project call.
//
// The selector is pinned down to its exact labels so that widening it takes a decision. A second
// selector, an extra matchLabels entry, or a matchExpressions clause is each another set of roles whose
// rules enter this one, and each would pass a check that only looked for the labels expected.
//
// The FILE COUNT of both directories is pinned too, fail-closed, and so is the model each file ships
// in, because TestPartnerDocRoleNamesExist treats every role published from them as one the backup
// integration document may name: an unpinned file added there would be simultaneously unpinned and legal
// to advertise to a reader.
//
// LIMIT: this describes the chart, and it recognizes a way in by its NAME — a label under that one
// prefix, an annotation, that one label. A future platform mechanism keyed on something else would
// pass. Nor does any of it reach the cluster: a label added to the live object is invisible here, and
// whether the aggregation controller filled this role at all — and with which rules — is past what a
// guard over a template can answer. The end-to-end vendor path binds the role and exercises it.
func TestBackupAgentAggregateRoleCollectsOneAxisAndSpreadsThroughNone(t *testing.T) {
	cases := []struct {
		scheme       rbacScheme
		dir          string
		name         string
		labels       map[string]string // exact for the new model, required for the legacy one
		exactLabels  bool
		selector     map[string]string
		rulesKeyless bool // no rules key at all, rather than an empty list
	}{
		{
			scheme:       schemeNew,
			dir:          backupAgentRoleDir(t),
			name:         backupAgentRoleName,
			labels:       map[string]string{"heritage": "deckhouse", "module": "state-snapshotter"},
			exactLabels:  true,
			selector:     map[string]string{backupAgentMemberLabel: "true", "rbac.deckhouse.io/kind": "capability"},
			rulesKeyless: true,
		},
		{
			scheme:   schemeLegacy,
			dir:      aggregateRolesDir(t),
			name:     legacyBackupAgentRoleName,
			labels:   map[string]string{"heritage": "deckhouse", "module": "state-snapshotter", "rbac.deckhouse.io/kind": "use"},
			selector: map[string]string{legacyBackupAgentLabel: "member", "rbac.deckhouse.io/kind": "use"},
		},
	}

	for _, tc := range cases {
		files := templateYAMLFiles(t, tc.dir)
		if len(files) != 1 {
			t.Fatalf("%s holds %d file(s), want exactly the one pinned here: %v — a role added there is unpinned, and being published from this directory it may already be named to a reader of the backup integration document",
				tc.dir, len(files), files)
		}
		file := files[0]

		for _, other := range rbacSchemes {
			if other == tc.scheme {
				continue
			}
			if extra := clusterRolesFromTemplate(t, file, other); len(extra) != 0 {
				t.Errorf("%s: the %s branch renders %d ClusterRole(s), want none — this file ships the collector of the %s model only, and a collector rendered into the other model selects on labels that model's capabilities do not carry",
					file, other, len(extra), tc.scheme)
			}
		}

		roles := clusterRolesFromTemplate(t, file, tc.scheme)
		if len(roles) != 1 {
			t.Fatalf("%s: the %s branch holds %d ClusterRole(s), want exactly one", file, tc.scheme, len(roles))
		}
		role := roles[0]

		if role.Name != tc.name {
			t.Errorf("%s: role is named %q, want %q — the name is what a reader of the document binds", file, role.Name, tc.name)
		}

		if tc.exactLabels {
			if !reflect.DeepEqual(role.Labels, tc.labels) {
				t.Errorf("%s: carries labels %v, want exactly %v — this role sits outside the role model, and every further label is either a road into it or a selector's match nobody decided",
					file, sortedLabelLines(role.Labels), sortedLabelLines(tc.labels))
			}
		} else {
			for key, value := range tc.labels {
				if got := role.Labels[key]; got != value {
					t.Errorf("%s: label %q is %q, want %q", file, key, got, value)
				}
			}
		}

		// No aggregation axis, whatever it is called, and not this module's membership label either.
		// This role is a collector, not a member of anything: a label of that kind on it composes the
		// entire set into somebody else's rights, and which sets exist is decided outside this
		// repository.
		labelsScanned := 0
		for key, value := range role.Labels {
			labelsScanned++
			if strings.HasPrefix(key, rbacAggregationAxisPrefix) || key == backupAgentMemberLabel {
				t.Errorf("%s: carries the aggregation label %s=%q — this role must carry none: it collects the whole permission set of an external backup product, and such a label hands that set to every holder of the corresponding access level at once, with nobody binding anything. The label to add is on the CAPABILITIES this role collects, never on this role",
					file, key, value)
			}
		}
		if got, labelled := role.Labels["rbac.deckhouse.io/delegatable"]; labelled {
			t.Errorf("%s: carries rbac.deckhouse.io/delegatable=%q, which this role must not have: it would put the role into the set a project owner may hand out inside their own project, and connecting an external backup product to the cluster is not a per-project decision",
				file, got)
		}

		// Annotations are refused outright, the same way and for the same reason as on the legacy
		// capabilities this role collects — with the boundary this role claims to hold making it worse
		// here: an access-level annotation is turned into bindings by a platform hook, cluster-wide ones
		// included, so the whole set would reach subjects nobody bound it to.
		for key, value := range role.Annotations {
			t.Errorf("%s: carries annotation %q=%q — a role that collects capabilities reaches a subject through an explicit binding and nothing else, and an annotation is another way in: the access levels of the cluster roles shipped here are assembled from one, and a hook turns it into bindings for that level's subjects",
				file, key, value)
		}

		switch {
		case tc.rulesKeyless && role.Rules != nil:
			t.Errorf("%s: declares a rules key (%d rule(s)), want none at all — the rules of this role are filled in the cluster from the roles its selector matches, and a rules key in the chart, even an empty one, makes server-side apply fight the aggregation controller", file, len(role.Rules))
		case len(role.Rules) != 0:
			t.Errorf("%s: declares %d rule(s), want none — the rules of this role are filled in the cluster from the roles its selector matches, so a rule written here is overwritten and describes nothing: %v",
				file, len(role.Rules), role.Rules)
		}

		selectorLabelsChecked := checkSingleSelector(t, file, role, tc.selector)

		t.Logf("checked the %s aggregate role %q in %s: %d label(s) required, %d label(s) scanned against the %q prefix, the membership label and rbac.deckhouse.io/delegatable, %d annotation(s) (all refused), %d rule(s) declared, %d selector label(s) compared",
			tc.scheme, role.Name, file, len(tc.labels), labelsScanned, rbacAggregationAxisPrefix, len(role.Annotations), len(role.Rules), selectorLabelsChecked)
		if labelsScanned == 0 || selectorLabelsChecked == 0 {
			t.Fatalf("scanned %d label(s) and compared %d selector label(s) — a zero on either side means the role was not read, not that it is clean",
				labelsScanned, selectorLabelsChecked)
		}
	}
}

// TestBackupAgentLegacyNameAliasCollectsWhatTheNewRoleDoes pins the alias that keeps the legacy name of
// the collecting role working on a 1.78 cluster, where the legacy object is no longer rendered. roleRef
// is immutable, so a RoleBinding made before the upgrade cannot be moved onto the new name; without the
// alias it would stay in place and grant nothing, and the first sign of it would be a client refused
// mid-backup.
//
// The alias follows the way user-authz keeps its own old role names: an empty ClusterRole whose
// aggregationRule is the new role's, the rbac.deckhouse.io/deprecated label, and the
// rbac.deckhouse.io/deprecated-replaced-by annotation naming the new role — without that annotation the
// platform ignores the alias with a warning, and a binding to it raises no deprecation alert. It is the
// one role here allowed annotations, and only those: the four console texts and the replacement.
//
// The selector is compared with the new role's own, not with a literal: the alias exists to grant what
// the new role grants, so the two drifting apart is the defect whichever of them moved.
//
// LIMIT: this pins the chart. That a binding to the alias raises the deprecation alert is the
// platform's behaviour and is not exercised here.
func TestBackupAgentLegacyNameAliasCollectsWhatTheNewRoleDoes(t *testing.T) {
	files := templateYAMLFiles(t, rbacv2CompatDir(t))
	if len(files) != 1 {
		t.Fatalf("templates/rbacv2-compat holds %d file(s), want exactly the alias pinned here: %v", len(files), files)
	}
	file := files[0]

	if legacy := clusterRolesFromTemplate(t, file, schemeLegacy); len(legacy) != 0 {
		t.Errorf("%s: the %s branch renders %d ClusterRole(s), want none — below 1.78 the legacy name is the real role, shipped from templates/rbacv2/use/roles, and an alias beside it would collide with it by name",
			file, schemeLegacy, len(legacy))
	}
	roles := clusterRolesFromTemplate(t, file, schemeNew)
	if len(roles) != 1 {
		t.Fatalf("%s: the %s branch holds %d ClusterRole(s), want exactly one", file, schemeNew, len(roles))
	}
	alias := roles[0]

	newRoles := clusterRolesFromTemplate(t, templateYAMLFiles(t, backupAgentRoleDir(t))[0], schemeNew)
	if len(newRoles) != 1 || newRoles[0].AggregationRule == nil {
		t.Fatalf("the new collecting role could not be read from templates/backup-agent — the alias has nothing to be compared with")
	}

	if alias.Name != legacyBackupAgentRoleName {
		t.Errorf("%s: the alias is named %q, want %q — it exists for the bindings made to that name", file, alias.Name, legacyBackupAgentRoleName)
	}
	wantLabels := map[string]string{
		"heritage":                     "deckhouse",
		"module":                       "state-snapshotter",
		"rbac.deckhouse.io/deprecated": "true",
	}
	if !reflect.DeepEqual(alias.Labels, wantLabels) {
		t.Errorf("%s: the alias carries labels %v, want exactly %v — like the role it stands for it sits outside the role model, and an aggregation or delegatable label would hand its set out past the bindings it exists for",
			file, sortedLabelLines(alias.Labels), sortedLabelLines(wantLabels))
	}

	wantAnnotations := append([]string{"rbac.deckhouse.io/deprecated-replaced-by"}, capabilityI18nAnnotations...)
	for key, value := range alias.Annotations {
		if !slices.Contains(wantAnnotations, key) {
			t.Errorf("%s: the alias carries annotation %q=%q — only the console texts and the replacement belong on it, and any other annotation is another way into somebody's rights", file, key, value)
		}
	}
	for _, key := range capabilityI18nAnnotations {
		if strings.TrimSpace(alias.Annotations[key]) == "" {
			t.Errorf("%s: the alias has no %s — the console shows it next to the deprecated name", file, key)
		}
	}
	if got := alias.Annotations["rbac.deckhouse.io/deprecated-replaced-by"]; got != backupAgentRoleName {
		t.Errorf("%s: rbac.deckhouse.io/deprecated-replaced-by is %q, want %q — without it the platform ignores the alias, and with another value it points the reader at the wrong role", file, got, backupAgentRoleName)
	}
	if !strings.Contains(alias.Annotations["en.meta.deckhouse.io/description"], backupAgentRoleName) ||
		!strings.Contains(alias.Annotations["ru.meta.deckhouse.io/description"], backupAgentRoleName) {
		t.Errorf("%s: a description of the alias does not name %s — it is where the console tells a reader what to migrate the binding to", file, backupAgentRoleName)
	}

	if alias.Rules != nil {
		t.Errorf("%s: the alias declares a rules key (%d rule(s)), want none at all — its rules belong to the aggregation controller", file, len(alias.Rules))
	}
	selectorLabelsChecked := checkSingleSelector(t, file, alias, newRoles[0].AggregationRule.ClusterRoleSelectors[0].MatchLabels)

	t.Logf("checked the alias %q in %s against %q: %d label(s), %d annotation(s), %d selector label(s) compared",
		alias.Name, file, backupAgentRoleName, len(alias.Labels), len(alias.Annotations), selectorLabelsChecked)
	if selectorLabelsChecked == 0 {
		t.Fatal("no selector label compared — zero findings means the alias was not read, not that it matches")
	}
}

// checkSingleSelector pins an aggregationRule to exactly one selector matching exactly the given labels,
// and returns how many selector labels it compared.
func checkSingleSelector(t *testing.T, file string, role rbacv1.ClusterRole, want map[string]string) int {
	t.Helper()

	switch {
	case role.AggregationRule == nil:
		t.Errorf("%s: declares no aggregationRule — the role then stays as empty as it is written here and a binding to it grants nothing at all", file)
		return 0
	case len(role.AggregationRule.ClusterRoleSelectors) != 1:
		t.Errorf("%s: aggregationRule holds %d selector(s), want exactly one — each further selector is an independent set of roles whose rules land in this one",
			file, len(role.AggregationRule.ClusterRoleSelectors))
		return 0
	}
	selector := role.AggregationRule.ClusterRoleSelectors[0]
	if len(selector.MatchExpressions) > 0 {
		t.Errorf("%s: the selector carries matchExpressions %v, which the comparison below does not model — an expression can match far more roles than a label equality does",
			file, selector.MatchExpressions)
	}
	if !reflect.DeepEqual(selector.MatchLabels, want) {
		t.Errorf("%s: the selector matches %v, want exactly %v — dropping a label widens the set of roles collected here, and adding one drops roles out of it with no error anywhere",
			file, selector.MatchLabels, want)
	}
	return len(selector.MatchLabels)
}

// TestBackupAgentAxisIsCarriedByExactlyThePinnedRoles pins the membership of the set an external
// backup product receives in one binding, in each model: which roles of this chart carry the membership
// label of that model, and — the half that is easy to leave out — that no other role of the chart
// carries it, nor the label of the OTHER model. One line of YAML is all it takes to add a role's rules
// to what the collector hands out, and templates/rbacv2/use/view.yaml already carries the selector's
// other label (rbac.deckhouse.io/kind, capability or use by model), so that line on that file would
// pour its grants into the set with nothing else to notice.
//
// SCOPE — every template under templates/rbacv2, templates/rbacv2-compat and templates/backup-agent,
// deliberately the whole of those trees and not just the directory the set lives in: the hole this
// closes is the label on a role nobody expects in the set, and a scan of that one directory could not
// see it. Those trees are also exactly as far as reading templates as objects goes — the only Helm
// actions in them are the model gate lines, which templateBranch resolves and fails the read on
// anything else, so the labels parsed out of a branch are the labels the cluster gets.
//
// The chart's other ClusterRoles — templates/rbac-for-us.yaml, templates/controller/rbac-for-us.yaml,
// templates/webhooks/rbac-for-us.yaml and templates/user-authz-cluster-roles.yaml — are out of scope,
// and not because scanning them would prove nothing: they cannot be scanned here at all. Their labels
// arrive from a Helm action, which templateBranch refuses, so pointing the walk at them would fail the
// test on the read rather than check anything. What templates/user-authz-cluster-roles.yaml grants is a
// separate question from the membership tracked here, and TestUseRolesDoNotGrantClusterScopedSnapshotLayer
// says where it draws its own scope.
//
// LIMIT, and the blind spot worth watching is this rather than those four files being skipped: ANY
// role outside the scanned trees that carries both selector labels joins the set unseen here — a
// static one because nothing walks it, a templated one because nothing here can read it. A role of
// this chart meant to be a member therefore belongs under templates/rbacv2; putting one anywhere else
// means widening the walk above in the same change, and for a templated file that is not a widening but
// a guard of another kind.
//
// SECOND LIMIT: this sees the templates of this chart, while the collector gathers any ClusterRole in
// the CLUSTER carrying both selector labels. The capability that completes the set — moving a volume's
// bytes, named in partnerDocForeignRoles — ships with another module, so that it carries the membership
// label is held by a guard in that module's repository and not by anything here.
func TestBackupAgentAxisIsCarriedByExactlyThePinnedRoles(t *testing.T) {
	members := []string{"view_snapshots", "manage_snapshots", "download_snapshots", "upload_snapshots", "restore_snapshots"}
	membership := map[rbacScheme]struct {
		label, value, prefix, otherLabel string
	}{
		schemeNew:    {backupAgentMemberLabel, "true", capabilityRolePrefix, legacyBackupAgentLabel},
		schemeLegacy: {legacyBackupAgentLabel, "member", legacyCapabilityRolePrefix, backupAgentMemberLabel},
	}

	repoRoot := filepath.Clean("../../../../..")
	var files []string
	for _, dir := range []string{
		filepath.Join(repoRoot, "templates", "rbacv2"),
		rbacv2CompatDir(t),
		backupAgentRoleDir(t),
	} {
		files = append(files, templateYAMLFiles(t, dir)...)
	}

	rolesScanned := 0
	for _, scheme := range rbacSchemes {
		m := membership[scheme]
		want := map[string]string{}
		for _, member := range members {
			want[m.prefix+member] = m.value
		}

		got := map[string]string{}
		for _, file := range files {
			for _, role := range clusterRolesFromTemplate(t, file, scheme) {
				rolesScanned++
				if value, labelled := role.Labels[m.otherLabel]; labelled {
					t.Errorf("%s (%s): role %q carries %s=%q, the membership label of the other model — nothing of this model selects on it, so it either misstates membership or waits for a collector nobody pinned",
						file, scheme, role.Name, m.otherLabel, value)
				}
				value, labelled := role.Labels[m.label]
				if !labelled {
					continue
				}
				if _, pinned := want[role.Name]; !pinned {
					t.Errorf("%s (%s): role %q carries %s=%q and is not one of the %d pinned members — its rules would be handed out with the set, so either it belongs there and belongs on the list in this test, or the label on it is the defect",
						file, scheme, role.Name, m.label, value, len(want))
					continue
				}
				got[role.Name] = value
			}
		}

		for name, value := range want {
			switch gotValue, member := got[name]; {
			case !member:
				t.Errorf("%s (%s) does not carry %s — the role then stays out of the set an external backup product is bound to, and the capability it grants is missing from that binding with no error at binding time",
					name, scheme, m.label)
			case gotValue != value:
				t.Errorf("%s (%s) carries %s=%q, want %q — the selector matches on the value, so any other value leaves the role out of the set", name, scheme, m.label, gotValue, value)
			}
		}
		t.Logf("%s: %d of %d pinned member(s) carry %s", scheme, len(got), len(want), m.label)
	}

	t.Logf("scanned %d file(s) and %d role(s) across %d model(s) for the backup-agent membership labels", len(files), rolesScanned, len(rbacSchemes))
	if rolesScanned == 0 {
		t.Fatal("no role scanned — zero findings means the walk stopped reading the templates, not that the set is intact")
	}
}

// TestBackupAgentSystemRoleGrantsOnlyDefinitionReads pins the one role of this chart an external backup
// product is bound to cluster-wide. Whatever it grants reaches every namespace at once, so its content is
// compared whole rather than checked for what it must contain: one rule, reading
// customsnapshotdefinitions, and nothing else. A definition names resource kinds and no object or data,
// which is why this read and no other is safe to hand out at that scope; the snapshot contents and the
// manifest checkpoints stay out of it, as TestUseRolesDoNotGrantClusterScopedSnapshotLayer keeps them
// out of the user roles.
//
// No aggregationRule, no label beyond heritage and module, no annotation: an aggregationRule would pour
// into this role whatever roles carry its selector, and a label or annotation is a road onto a platform
// level or into a project owner's delegation set, either of which hands the role out with nobody binding
// it. The two models must render the same object, since the document gives one binding for both.
//
// LIMIT: this describes the chart. That the role is bound only by whoever connects a backup product, and
// that the live object matches the template, are past what a guard over a template can answer.
func TestBackupAgentSystemRoleGrantsOnlyDefinitionReads(t *testing.T) {
	files := templateYAMLFiles(t, backupAgentSystemRoleDir(t))
	if len(files) != 1 {
		t.Fatalf("%s holds %d file(s), want exactly the one pinned here: %v — a role added there is unpinned, and being published from this directory it may already be named to a reader of the backup integration document",
			backupAgentSystemRoleDir(t), len(files), files)
	}
	file := files[0]

	wantLabels := map[string]string{"heritage": "deckhouse", "module": "state-snapshotter"}
	wantRules := []rbacv1.PolicyRule{{
		APIGroups: []string{"state-snapshotter.deckhouse.io"},
		Resources: []string{"customsnapshotdefinitions"},
		Verbs:     []string{"get", "list", "watch"},
	}}

	rendered := map[rbacScheme]rbacv1.ClusterRole{}
	for _, scheme := range rbacSchemes {
		roles := clusterRolesFromTemplate(t, file, scheme)
		if len(roles) != 1 {
			t.Fatalf("%s: the %s branch holds %d ClusterRole(s), want exactly one — the document gives one binding for every platform version, so both models must ship the role", file, scheme, len(roles))
		}
		role := roles[0]
		rendered[scheme] = role

		if role.Name != backupAgentSystemRoleName {
			t.Errorf("%s (%s): role is named %q, want %q — the name is what a reader of the document binds", file, scheme, role.Name, backupAgentSystemRoleName)
		}
		if !reflect.DeepEqual(role.Labels, wantLabels) {
			t.Errorf("%s (%s): carries labels %v, want exactly %v — a role bound cluster-wide must not be reachable through an aggregation axis or a delegation set",
				file, scheme, sortedLabelLines(role.Labels), sortedLabelLines(wantLabels))
		}
		for key, value := range role.Annotations {
			t.Errorf("%s (%s): carries annotation %q=%q — a platform hook turns an access-level annotation into bindings, which would hand a cluster-wide role to subjects nobody bound it to", file, scheme, key, value)
		}
		if role.AggregationRule != nil {
			t.Errorf("%s (%s): declares an aggregationRule — its rules would be replaced by those of every role its selector matches, and every one of them would reach every namespace", file, scheme)
		}
		if !reflect.DeepEqual(role.Rules, wantRules) {
			t.Errorf("%s (%s): grants %v, want exactly %v — whatever this role grants, it grants in every namespace at once", file, scheme, role.Rules, wantRules)
		}
	}

	if !reflect.DeepEqual(rendered[schemeNew], rendered[schemeLegacy]) {
		t.Errorf("%s: the two models render different objects — the document names one role and one binding for every platform version", file)
	}

	t.Logf("checked %q in %s across %d model(s): %d label(s), %d rule(s)", backupAgentSystemRoleName, file, len(rendered), len(wantLabels), len(wantRules))
	if len(rendered) != len(rbacSchemes) {
		t.Fatalf("read the role in %d of %d model(s) — a model not read is not a model checked", len(rendered), len(rbacSchemes))
	}
}

// TestUseRolesDoNotGrantClusterScopedSnapshotLayer keeps the cluster-scoped layer of this module out
// of the roles handed to users, in both models. A client reads and writes namespaced objects and
// namespaced subresources; the snapshot contents, the manifest checkpoints and their chunks, and the
// snapshot definitions are the module's internals, and raw chunk data is the most sensitive of them. In
// the new model a cluster-scoped rule in a namespace capability would grant nothing at all, which is no
// reason to let one in: it would still say something the role does not do.
//
// The prefixes are SINGULAR on purpose: the chunk resource is named manifestcheckpointcontentchunks,
// which the plural "manifestcheckpoints" does not cover — the exact hole this test exists to close.
// A prefix also covers every subresource of the resource (snapshotcontents/manifests-download,
// manifestcheckpoints/manifests). No namespaced resource of this module starts with one of them.
//
// SCOPE — templates/rbacv2/use/** only. templates/user-authz-cluster-roles.yaml grants of these
// resources only read of snapshotcontents to ClusterEditor (pinned by TestUserAuthzRolesGrantExactRules),
// and templates/rbacv2/manage/** grants read of manifestcheckpoints to the cluster-level storage viewer
// on purpose, so widening the walk to them is not the fix if it ever fails elsewhere.
//
// A wildcard is rejected outright, in both the resource and the group: "*" and "*/<subresource>" are
// how the authorizer spells "everything", so a prefix comparison cannot see through one — a single
// wildcard rule hands over the whole cluster-scoped layer while matching no forbidden prefix at all.
//
// LIMIT: it catches the cluster-scoped resources of THIS module, not every cluster-scoped resource in
// the cluster. The CSI group's volumesnapshotcontents and volumesnapshotclasses match no prefix here,
// so granting one of those to a user role would pass this test and needs a decision of its own.
func TestUseRolesDoNotGrantClusterScopedSnapshotLayer(t *testing.T) {
	clusterScopedPrefixes := []string{"snapshotcontent", "manifestcheckpoint", "customsnapshotdefinition"}

	useDir := filepath.Join(filepath.Clean("../../../../.."), "templates", "rbacv2", "use")
	files := templateYAMLFiles(t, useDir)

	var rolesScanned, resourcesScanned int
	for _, scheme := range rbacSchemes {
		for _, file := range files {
			for _, role := range clusterRolesFromTemplate(t, file, scheme) {
				rolesScanned++
				for _, rule := range role.Rules {
					for _, group := range rule.APIGroups {
						if strings.Contains(group, "*") {
							t.Errorf("%s (%s): role %q grants %v on API group %q — a wildcard group covers this module's cluster-scoped layer too, so it is refused here whatever the resources say",
								file, scheme, role.Name, rule.Verbs, group)
						}
					}
					for _, resource := range rule.Resources {
						resourcesScanned++
						if strings.Contains(resource, "*") {
							t.Errorf("%s (%s): role %q grants %v on %q — a wildcard resource (\"*\" or \"*/<subresource>\") reaches this module's cluster-scoped layer, and no prefix comparison can see through it",
								file, scheme, role.Name, rule.Verbs, resource)
							continue
						}
						for _, prefix := range clusterScopedPrefixes {
							if strings.HasPrefix(resource, prefix) {
								t.Errorf("%s (%s): role %q grants %v on %q, which belongs to this module's cluster-scoped layer — user roles address namespaced objects and namespaced subresources only",
									file, scheme, role.Name, rule.Verbs, resource)
							}
						}
					}
				}
			}
		}
	}

	t.Logf("scanned %d file(s) in %d model(s), %d role(s) and %d resource grant(s) under templates/rbacv2/use against %d forbidden prefix(es), wildcards refused",
		len(files), len(rbacSchemes), rolesScanned, resourcesScanned, len(clusterScopedPrefixes))
	if resourcesScanned == 0 {
		t.Fatal("no resource grant scanned under templates/rbacv2/use — zero findings means the scan stopped reading the roles, not that they are clean")
	}
}

func TestUserAuthzRolesGrantExactRules(t *testing.T) {
	path := filepath.Join(filepath.Clean("../../../../.."), "templates", "user-authz-cluster-roles.yaml")
	type role struct {
		level  string
		grants []string
	}
	want := map[string]role{
		"d8:user-authz:state-snapshotter:user": {"User", []string{
			"state-snapshotter.deckhouse.io/manifestcapturerequests: get,list,watch",
			"state-snapshotter.deckhouse.io/snapshots/status: get,list,watch",
			"state-snapshotter.deckhouse.io/snapshots: get,list,watch",
		}},
		"d8:user-authz:state-snapshotter:editor": {"Editor", []string{
			"state-snapshotter.deckhouse.io/snapshots: create,delete",
		}},
		"d8:user-authz:state-snapshotter:admin": {"Admin", []string{
			"subresources.snapshot.storage.k8s.io/volumesnapshots/manifests-and-children-refs-upload: create",
			"subresources.snapshot.storage.k8s.io/volumesnapshots/manifests-download: get",
			"subresources.snapshot.storage.k8s.io/volumesnapshots/manifests-with-data-restoration: get",
			"subresources.state-snapshotter.deckhouse.io/snapshots/manifests-and-children-refs-upload: create",
			"subresources.state-snapshotter.deckhouse.io/snapshots/manifests-download: get",
			"subresources.state-snapshotter.deckhouse.io/snapshots/manifests-with-data-restoration: get",
			"subresources.storage-foundation.deckhouse.io/volumesnapshots/manifests-and-children-refs-upload: create",
			"subresources.storage-foundation.deckhouse.io/volumesnapshots/manifests-download: get",
			"subresources.storage-foundation.deckhouse.io/volumesnapshots/manifests-with-data-restoration: get",
		}},
		"d8:user-authz:state-snapshotter:cluster-editor": {"ClusterEditor", []string{
			"state-snapshotter.deckhouse.io/snapshotcontents: get,list,watch",
		}},
	}

	var kept []string
	for _, line := range strings.Split(readTemplate(t, path), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "{{") {
			kept = append(kept, line)
		}
	}
	seen := map[string]bool{}
	for _, doc := range strings.Split(strings.Join(kept, "\n"), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var got rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &got); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		exp, ok := want[got.Name]
		if !ok {
			t.Errorf("%s: ClusterRole %q is not pinned here", path, got.Name)
			continue
		}
		seen[got.Name] = true
		if level := got.Annotations["user-authz.deckhouse.io/access-level"]; level != exp.level {
			t.Errorf("%s: ClusterRole %q has access level %q, want %q", path, got.Name, level, exp.level)
		}
		if lines := sortedGrantLines(flattenPolicyRules(got.Rules)); !reflect.DeepEqual(lines, exp.grants) {
			t.Errorf("%s: ClusterRole %q grants\n  %s\nwant\n  %s", path, got.Name, strings.Join(lines, "\n  "), strings.Join(exp.grants, "\n  "))
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("%s holds no ClusterRole %q", path, name)
		}
	}
}

// TestEveryNamespacedSubresourceRouteHasCapabilityRole is the guard against the drift "an endpoint was
// added and the rights for it were not": every namespaced aggregated subresource this module serves
// must be reachable with the capability roles it publishes, in each model, with the verb the endpoint is
// authorized under (the aggregated API server authorizes each request with a SubjectAccessReview built
// from the URL, so GET reads as "get" and POST as "create").
//
// Coverage is measured against the capability roles alone. The older use/view and use/edit roles cover
// a different resource entirely and are no part of this contract, so a subresource reachable only
// through them still counts as uncovered.
//
// SOURCE of the served set: the module's own aggregated discovery documents, obtained by registering
// the real routes on a mux and requesting them — not a list of endpoint names copied into this file,
// which would only ever pin the copy. Discovery also carries the scope flag and the verbs, which is
// exactly what has to be compared against RBAC.
//
// LIMIT: discovery is the ADVERTISED set, and this guard is only worth as much as the claim that it
// equals the SERVED one. That equality is pinned separately and in both directions by
// TestRoutedSubresourcesMatchDiscovery, which reads the case values of the namespaced subresource
// routers themselves — so a route added without a discovery entry fails there rather than passing
// unnoticed here. Cluster-scoped routes are outside both guards' subject, and outside these roles'.
//
// SECOND LIMIT, and the one that grew with the volume node moving out: this compares served → granted
// only. The roles also grant endpoints that ANOTHER module serves, which no discovery reachable from here
// advertises, so nothing checks those against a server; their presence is pinned by name in
// TestUserCapabilityRolesGrantExactRulesAndLabels and their reachability by the end-to-end suite.
func TestEveryNamespacedSubresourceRouteHasCapabilityRole(t *testing.T) {
	capabilityFiles := capabilityRoleFiles(t)
	served := servedAggregatedResources(t)

	var namespacedChecked, verbsChecked, clusterScopedSkipped int
	for _, scheme := range rbacSchemes {
		granted := map[string]map[string]bool{}
		for _, file := range capabilityFiles {
			for _, role := range clusterRolesFromTemplate(t, file, scheme) {
				for key, verbs := range flattenPolicyRules(role.Rules) {
					if granted[key] == nil {
						granted[key] = map[string]bool{}
					}
					for _, verb := range verbs {
						granted[key][verb] = true
					}
				}
			}
		}

		for _, list := range served {
			group, _, _ := strings.Cut(list.GroupVersion, "/")
			for _, resource := range list.APIResources {
				if !resource.Namespaced {
					clusterScopedSkipped++
					continue
				}
				namespacedChecked++
				key := group + "/" + resource.Name
				for _, verb := range resource.Verbs {
					verbsChecked++
					if !granted[key][verb] {
						t.Errorf("%s serves %q for verb %q, which no capability role of the %s model grants — add it to a capability under templates/rbacv2/use, in both branches (a served endpoint nobody can be granted is unusable)",
							list.GroupVersion, resource.Name, verb, scheme)
					}
				}
			}
		}
	}

	t.Logf("checked %d namespaced subresource(s) and %d verb(s) advertised by the aggregated discovery documents against %d capability role file(s) in %d model(s); skipped %d cluster-scoped entry(ies)",
		namespacedChecked, verbsChecked, len(capabilityFiles), len(rbacSchemes), clusterScopedSkipped)
	if verbsChecked == 0 {
		t.Fatal("no namespaced subresource verb checked — zero findings means discovery stopped being read, not that every route is covered")
	}
}

// servedAggregatedResources returns the discovery document of the aggregated group this module serves,
// read through the routes the server actually registers. The discovery URL is spelled out because the
// group and version are registered as a literal mux prefix and exported nowhere; a rename makes this
// request 404 and fails the caller loudly instead of leaving it to validate against a set that is no
// longer served.
func servedAggregatedResources(t *testing.T) []metav1.APIResourceList {
	t.Helper()

	log := logger.NewNop()

	// Discovery touches neither the client nor the usecases, so nil dependencies are enough.
	mux := http.NewServeMux()
	api.NewArchiveHandler(nil, nil, log).SetupRoutes(mux)

	var lists []metav1.APIResourceList
	for _, path := range []string{
		"/apis/subresources.state-snapshotter.deckhouse.io/v1alpha1",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("discovery %s answered %d, want 200: that group/version is no longer served, so update it here and in the roles", path, rec.Code)
		}
		var list metav1.APIResourceList
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode discovery %s: %v", path, err)
		}
		if list.GroupVersion == "" || len(list.APIResources) == 0 {
			t.Fatalf("discovery %s advertises nothing (groupVersion=%q, %d resources)", path, list.GroupVersion, len(list.APIResources))
		}
		lists = append(lists, list)
	}
	return lists
}

func capabilityRolesDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Clean("../../../../.."), "templates", "rbacv2", "use")
}

// capabilityRoleFiles lists the templates of the capabilities of their own: the files directly under
// templates/rbacv2/use, where rbac.yaml writes them, without the view and edit levels beside them and
// without the collecting role under roles/.
func capabilityRoleFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(capabilityRolesDir(t))
	if err != nil {
		t.Fatalf("read %s: %v", capabilityRolesDir(t), err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".yaml") || name == "view.yaml" || name == "edit.yaml" {
			continue
		}
		files = append(files, filepath.Join(capabilityRolesDir(t), name))
	}
	return files
}

// aggregateRolesDir holds the legacy role that COLLECTS capabilities instead of granting them. It is a
// helper of its own and not a second entry in capabilityRolesDir: that one feeds the guards which
// count and pin capabilities, and a collector is not a capability — folding the two would break those
// counts and pull the collector into a coverage comparison it has no place in.
func aggregateRolesDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Clean("../../../../.."), "templates", "rbacv2", "use", "roles")
}

// backupAgentRoleDir holds the collecting role of the new model. It is outside templates/rbacv2 because
// the role is outside the role model: the platform's contract reads that tree as capabilities and
// roles of its own, and this one is neither.
func backupAgentRoleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Clean("../../../../.."), "templates", "backup-agent")
}

// backupAgentSystemRoleDir holds the cluster-scoped role of an external backup product. It is outside
// templates/rbacv2 for the reason backupAgentRoleDir is, and apart from it because that directory's
// file count is pinned to the collecting role alone.
func backupAgentSystemRoleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Clean("../../../../.."), "templates", "backup-agent-system")
}

// rbacv2CompatDir holds the alias that keeps the legacy name of the collecting role alive on a 1.78
// cluster.
func rbacv2CompatDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Clean("../../../../.."), "templates", "rbacv2-compat")
}

// templateBranch returns the text of an RBAC template that the chart renders in the given model. The
// templates read by these guards are plain YAML behind one gate on the model, in one of three shapes:
//
//	eq-gate, new object, else, legacy object, end — both models ship the role (what the migration script writes)
//	eq-gate, new object, end                      — the new model only
//	ne-gate, legacy object, end                   — the legacy model only
//
// Each gate line must be one of the exact lines above and the only Helm actions in the file. Anything
// else fails the read instead of being stripped: a Helm action this reader does not model can put a
// label, a rule or a whole document into what the cluster gets, and a guard that dropped it would be
// describing a file the chart does not render. A file with no gate fails too — it would ship the same
// object into both models, and one of them then carries labels its platform does not read.
func templateBranch(t *testing.T, path string, scheme rbacScheme) string {
	t.Helper()

	branch, err := splitTemplateBranch(readTemplate(t, path), scheme)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return branch
}

// splitTemplateBranch is templateBranch over the text of a template, so the reader can be pinned
// against planted shapes without a file or a failing test in between.
func splitTemplateBranch(content string, scheme rbacScheme) (string, error) {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	var actions []int
	for i, line := range lines {
		if strings.Contains(line, "{{") || strings.Contains(line, "}}") {
			actions = append(actions, i)
		}
	}
	last := len(lines) - 1
	join := func(part []string) string {
		if len(part) == 0 {
			return ""
		}
		return strings.Join(part, "\n") + "\n"
	}

	switch {
	case len(actions) == 3 && actions[0] == 0 && actions[2] == last &&
		lines[0] == rbacGateNewScheme && lines[actions[1]] == rbacGateElse && lines[last] == rbacGateEnd:
		if scheme == schemeNew {
			return join(lines[1:actions[1]]), nil
		}
		return join(lines[actions[1]+1 : last]), nil
	case len(actions) == 2 && actions[0] == 0 && actions[1] == last &&
		lines[0] == rbacGateNewScheme && lines[last] == rbacGateEnd:
		if scheme == schemeNew {
			return join(lines[1:last]), nil
		}
		return "", nil
	case len(actions) == 2 && actions[0] == 0 && actions[1] == last &&
		lines[0] == rbacGateLegacyScheme && lines[last] == rbacGateEnd:
		if scheme == schemeLegacy {
			return join(lines[1:last]), nil
		}
		return "", nil
	}

	var found []string
	for _, i := range actions {
		found = append(found, strings.TrimSpace(lines[i]))
	}
	return "", fmt.Errorf("the Helm actions %q are not one of the model gates these guards read (%q [+ %q] + %q, or %q + %q) — a template here is plain YAML behind exactly one such gate, and anything else would be stripped from what the guards see while it still reaches the cluster",
		found, rbacGateNewScheme, rbacGateElse, rbacGateEnd, rbacGateLegacyScheme, rbacGateEnd)
}

// TestTemplateBranchReadsOnlyTheGateShapesItModels pins the reader every guard above stands on,
// against the shapes it must take apart and the ones it must refuse. The refusals are the point: a
// reader that silently accepted an unmodelled action would make every guard above describe a file the
// chart does not render.
func TestTemplateBranchReadsOnlyTheGateShapesItModels(t *testing.T) {
	for name, tc := range map[string]struct {
		body                string
		wantNew, wantLegacy string
	}{
		"both models": {rbacGateNewScheme + "\nkind: New\n" + rbacGateElse + "\nkind: Legacy\n" + rbacGateEnd + "\n", "kind: New\n", "kind: Legacy\n"},
		"new only":    {rbacGateNewScheme + "\nkind: New\n" + rbacGateEnd + "\n", "kind: New\n", ""},
		"legacy only": {rbacGateLegacyScheme + "\nkind: Legacy\n" + rbacGateEnd + "\n", "", "kind: Legacy\n"},
	} {
		for scheme, want := range map[rbacScheme]string{schemeNew: tc.wantNew, schemeLegacy: tc.wantLegacy} {
			got, err := splitTemplateBranch(tc.body, scheme)
			if err != nil || got != want {
				t.Errorf("%s, %s branch: got %q (error %v), want %q", name, scheme, got, err, want)
			}
		}
	}

	refusals := map[string]string{
		"no gate":                    "kind: Both\n",
		"an action inside a branch":  rbacGateNewScheme + "\nkind: New\n  {{- include \"labels\" . }}\n" + rbacGateEnd + "\n",
		"a gate on something else":   "{{- if .Values.enabled }}\nkind: New\n" + rbacGateEnd + "\n",
		"a document after the gate":  rbacGateNewScheme + "\nkind: New\n" + rbacGateEnd + "\n---\nkind: Both\n",
		"an else behind the ne-gate": rbacGateLegacyScheme + "\nkind: Legacy\n" + rbacGateElse + "\nkind: New\n" + rbacGateEnd + "\n",
	}
	for name, body := range refusals {
		for _, scheme := range rbacSchemes {
			if got, err := splitTemplateBranch(body, scheme); err == nil {
				t.Errorf("%s: the %s branch was read as %q — a template this reader does not model must fail the read, not be stripped", name, scheme, got)
			}
		}
	}
}

// clusterRolesFromTemplate parses every document of one model's branch of an RBAC template file (see
// templateBranch); a branch the file does not ship yields no role.
//
// A document that is not a ClusterRole fails the read instead of being skipped, and that is the whole
// point of the check: the guards above count roles and files, so a second document appended to a file
// they already pin — a ClusterRoleBinding handing a capability to every authenticated subject, say —
// would otherwise pass through all of them unseen. Something else genuinely belonging here means
// teaching these guards about it, not widening this parse.
func clusterRolesFromTemplate(t *testing.T, path string, scheme rbacScheme) []rbacv1.ClusterRole {
	t.Helper()

	var roles []rbacv1.ClusterRole
	for i, doc := range strings.Split(templateBranch(t, path, scheme), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			t.Fatalf("parse %s (%s): %v", path, scheme, err)
		}
		if role.Kind != "ClusterRole" {
			t.Fatalf("%s (%s): document %d is a %q named %q, and these RBAC templates hold ClusterRoles only — a document of any other kind is invisible to the guards that count roles and files here",
				path, scheme, i+1, role.Kind, role.Name)
		}
		roles = append(roles, role)
	}
	return roles
}

func sortedLabelLines(labels map[string]string) []string {
	lines := make([]string, 0, len(labels))
	for key, value := range labels {
		lines = append(lines, key+"="+value)
	}
	sort.Strings(lines)
	return lines
}

// flattenPolicyRules turns rules into one "<apiGroup>/<resource>" key per granted pair with its verbs
// sorted and deduplicated, so how the YAML groups its lists is not part of the comparison.
func flattenPolicyRules(rules []rbacv1.PolicyRule) map[string][]string {
	verbsOf := map[string]map[string]bool{}
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				key := group + "/" + resource
				if verbsOf[key] == nil {
					verbsOf[key] = map[string]bool{}
				}
				for _, verb := range rule.Verbs {
					verbsOf[key][verb] = true
				}
			}
		}
	}

	grants := make(map[string][]string, len(verbsOf))
	for key, verbs := range verbsOf {
		list := make([]string, 0, len(verbs))
		for verb := range verbs {
			list = append(list, verb)
		}
		sort.Strings(list)
		grants[key] = list
	}
	return grants
}

func sortedGrantLines(grants map[string][]string) []string {
	lines := make([]string, 0, len(grants))
	for key, verbs := range grants {
		lines = append(lines, key+": "+strings.Join(verbs, ","))
	}
	sort.Strings(lines)
	return lines
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
