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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/deckhouse/state-snapshotter/images/state-snapshotter-controller/internal/api"
	"github.com/deckhouse/state-snapshotter/lib/go/common/pkg/logger"
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
// User capability roles and the role that collects them —
// templates/rbacv2/use/capabilities/*.yaml and templates/rbacv2/use/roles/*.yaml
//
// Plain testing guards reading the chart from disk rather than Ginkgo specs over embedded fixtures:
// the subject of every check below IS the template file the chart ships, so an embedded copy would be
// a copy checking itself and a passing test would say nothing about what the cluster gets.
// ---------------------------------------------------------------------------

const (
	// capabilityRolePrefix is the name every user capability role of this module carries; the file
	// base name is the capability that follows it.
	capabilityRolePrefix = "d8:use:capability:module:state-snapshotter:"

	// rbacAggregationLabel is the cluster-level axis. A role carrying it is handed to everyone
	// already holding the matching cluster-wide role, with nobody binding anything, so its presence
	// is part of the contract and is pinned per role below.
	rbacAggregationLabel = "rbac.deckhouse.io/aggregate-to-kubernetes-as"

	// rbacBackupAgentLabel is the axis that composes the capabilities an external backup product
	// needs into one role for it to be bound to. It is not a second road onto a cluster-wide level,
	// and that difference is why a capability may carry this axis while deliberately refusing the one
	// above: the role collecting this axis carries no axis of its own and grants nothing at all until
	// a RoleBinding names it, whereas rbacAggregationLabel hands the capability to every holder of a
	// level the moment the label appears. Written down because the flat ban this replaced looked
	// right, and restoring it would take the single-binding path away.
	rbacBackupAgentLabel = "rbac.deckhouse.io/aggregate-to-backup-agent-as"

	// rbacAggregationAxisPrefix is what every aggregation axis of this platform is named with. The
	// guards below compare against the PREFIX and not against a list of axis names, because the list
	// is not this repository's to know: the platform ships axes of its own, and a role of this module
	// carrying one of them is composed into a set assembled elsewhere, on terms nothing here states.
	rbacAggregationAxisPrefix = "rbac.deckhouse.io/aggregate-"

	// backupAgentRoleName is the role that collects rbacBackupAgentLabel. It lives under
	// templates/rbacv2/use/roles and not among the capabilities because a role with an
	// aggregationRule inside that directory is refused below, for a reason that still holds.
	backupAgentRoleName = capabilityRolePrefix + "backup_agent"
)

// TestUserCapabilityRolesGrantExactRulesAndLabels pins every user capability role by name: the exact
// set of "<apiGroup>/<resource>" grants with their verbs, BOTH aggregation axes — including the two
// roles that deliberately stay off the cluster-level one — and that nothing else composes the role
// into somebody's rights. Rules are compared flattened, so regrouping the YAML is free while adding
// or widening a grant is not.
//
// TWO AXES, and the difference between them is why one is allowed where the other is refused:
// rbacAggregationLabel puts the capability into a cluster-wide access level, so every holder of that
// level gains it with nobody binding anything; rbacBackupAgentLabel puts it into a role that carries
// no axis of its own and grants nothing until a RoleBinding names it. Both are pinned per role below,
// in both directions — an axis on a role that must not have it fails here, and so does an axis
// missing from a role that must.
//
// LIMIT: this compares what the role SAYS. That the granted subresources are the ones the server
// actually serves is a different question, checked by TestEveryNamespacedSubresourceRouteHasCapabilityRole.
func TestUserCapabilityRolesGrantExactRulesAndLabels(t *testing.T) {
	expected := map[string]struct {
		aggregateAs   string // "" — this role deliberately stays off the cluster-level axis
		backupAgentAs string // "" — this role is deliberately not part of the backup-agent set
		grants        map[string][]string
	}{
		"view_snapshots": {
			aggregateAs:   "viewer",
			backupAgentAs: "member",
			grants: map[string][]string{
				"state-snapshotter.deckhouse.io/snapshots":        {"get", "list", "watch"},
				"state-snapshotter.deckhouse.io/snapshots/status": {"get", "list", "watch"},
				// A tree's volume nodes are addressed as VolumeSnapshots, and their status is where a
				// walk reads the bound content from — without it the tree cannot be traversed.
				"snapshot.storage.k8s.io/volumesnapshots": {"get", "list", "watch"},
			},
		},
		"manage_snapshots": {
			aggregateAs:   "manager",
			backupAgentAs: "member",
			// No update/patch: a Snapshot spec is one-shot and immutable.
			grants: map[string][]string{
				"state-snapshotter.deckhouse.io/snapshots": {"create", "delete"},
			},
		},
		"download_snapshots": {
			aggregateAs:   "manager",
			backupAgentAs: "member",
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
			// Off the cluster-level axis for a reason of its own: this capability writes into ANOTHER
			// module's resource, so that axis would let every manager in the cluster create
			// VolumeSnapshots merely because this module is installed. In the backup-agent set all the
			// same — loading a tree back into the cluster is half of what a backup product does — and
			// that set reaches an explicitly bound subject only.
			aggregateAs:   "",
			backupAgentAs: "member",
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
			// Off the cluster-level axis: this capability reads a whole subtree already rewritten for
			// a target namespace, which is not something a holder of a general cluster-wide role is
			// assumed to have asked for. In the backup-agent set, because restoring is the other half
			// of what a backup product does, and membership there grants nothing until somebody binds
			// the collecting role in a namespace on purpose.
			aggregateAs:   "",
			backupAgentAs: "member",
			grants: map[string][]string{
				"subresources.state-snapshotter.deckhouse.io/snapshots/manifests-with-data-restoration":        {"get"},
				"subresources.storage-foundation.deckhouse.io/volumesnapshots/manifests-with-data-restoration": {"get"},
				// The deprecated group of the volume node's endpoints; see download_snapshots above.
				"subresources.snapshot.storage.k8s.io/volumesnapshots/manifests-with-data-restoration": {"get"},
			},
		},
	}

	files := templateYAMLFiles(t, capabilityRolesDir(t))
	if len(files) != len(expected) {
		t.Fatalf("templates/rbacv2/use/capabilities holds %d file(s) but %d capability role(s) are pinned here: %v",
			len(files), len(expected), files)
	}

	grantsChecked := 0
	for _, file := range files {
		capability := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		want, pinned := expected[capability]
		if !pinned {
			t.Errorf("%s: capability %q is published with no expectation here — every capability this module hands out is pinned by name, so add its grants and aggregation label above", file, capability)
			continue
		}

		roles := clusterRolesFromTemplate(t, file)
		if len(roles) != 1 {
			t.Errorf("%s: holds %d ClusterRole(s), want exactly one", file, len(roles))
			continue
		}
		role := roles[0]

		if want := capabilityRolePrefix + capability; role.Name != want {
			t.Errorf("%s: role is named %q, want %q (the file base name is the capability)", file, role.Name, want)
		}
		for key, value := range map[string]string{
			"heritage":               "deckhouse",
			"module":                 "state-snapshotter",
			"rbac.deckhouse.io/kind": "use",
		} {
			if got := role.Labels[key]; got != value {
				t.Errorf("%s: label %q is %q, want %q", file, key, got, value)
			}
		}
		// A capability reaches somebody's rights by exactly two mechanisms: the two aggregation axes
		// pinned above. Three others would do it quietly, and all three are refused outright rather
		// than pinned per role — a THIRD rbac.deckhouse.io/aggregate-* axis, whose collecting role
		// this repository does not describe and whose reach therefore nothing here bounds; any
		// annotation (the access levels of the cluster roles this module ships are assembled from an
		// annotation, so one access-level annotation on a role that must not be aggregated hands it to
		// an entire level); and an aggregationRule on the capability itself, which makes the controller
		// overwrite rules from whatever matches the selector — after which the comparison below
		// describes the file and not the role the cluster ends up with. That last ban is why the role
		// collecting rbacBackupAgentLabel lives outside this directory instead of among the
		// capabilities it collects.
		if role.AggregationRule != nil {
			t.Errorf("%s: declares an aggregationRule — its rules would then be rewritten by the controller from the roles the selector matches, and everything pinned below would stop describing the effective role", file)
		}
		for key := range role.Annotations {
			t.Errorf("%s: carries annotation %q — a capability role is composed into somebody's rights through its aggregation label and nothing else, and an annotation is another way in", file, key)
		}
		for key := range role.Labels {
			if strings.HasPrefix(key, rbacAggregationAxisPrefix) &&
				key != rbacAggregationLabel && key != rbacBackupAgentLabel {
				t.Errorf("%s: carries a third aggregation axis %q — the capabilities of this module go onto %s and %s only, and an axis described nowhere in this repository composes them into rights nothing here bounds",
					file, key, rbacAggregationLabel, rbacBackupAgentLabel)
			}
		}

		got, labelled := role.Labels[rbacAggregationLabel]
		switch {
		case want.aggregateAs == "" && labelled:
			t.Errorf("%s: carries %s=%q, but this capability must stay off the cluster-level axis — the label would hand it to every holder of that cluster-wide role, silently", file, rbacAggregationLabel, got)
		case want.aggregateAs != "" && got != want.aggregateAs:
			t.Errorf("%s: aggregates as %q, want %q", file, got, want.aggregateAs)
		}

		gotAgent, agentLabelled := role.Labels[rbacBackupAgentLabel]
		switch {
		case want.backupAgentAs == "" && agentLabelled:
			t.Errorf("%s: carries %s=%q, but this capability is not pinned as part of the backup-agent set — the label adds its grants to the one role an external backup product is bound to", file, rbacBackupAgentLabel, gotAgent)
		case want.backupAgentAs != "" && gotAgent != want.backupAgentAs:
			t.Errorf("%s: is in the backup-agent set as %q, want %q — the selector of %s matches on the value, so anything else leaves this capability out of the single binding and its absence surfaces only when a client is refused",
				file, gotAgent, want.backupAgentAs, backupAgentRoleName)
		}

		for _, rule := range role.Rules {
			if len(rule.ResourceNames) > 0 || len(rule.NonResourceURLs) > 0 {
				t.Errorf("%s: a rule narrows to resourceNames/nonResourceURLs %v/%v, which the flattened comparison below does not model — pin it explicitly or drop it",
					file, rule.ResourceNames, rule.NonResourceURLs)
			}
		}
		if grants := flattenPolicyRules(role.Rules); !reflect.DeepEqual(grants, want.grants) {
			t.Errorf("%s: grants\n  %v\nwant\n  %v", file, sortedGrantLines(grants), sortedGrantLines(want.grants))
		}
		grantsChecked += len(want.grants)
	}

	t.Logf("checked %d capability role(s) and %d grant(s) under templates/rbacv2/use/capabilities", len(files), grantsChecked)
	if grantsChecked == 0 {
		t.Fatal("no grant checked — zero findings means the roles stopped being read, not that they are correct")
	}
}

// TestBackupAgentAggregateRoleCollectsOneAxisAndSpreadsThroughNone pins the role an external backup
// product is bound to: its name, the labels it carries, the two it must not, that its rules are empty
// in the chart, and that its aggregationRule is exactly one selector over exactly two labels.
//
// WHAT IS ABSENT is the security condition of the whole arrangement, so it is pinned by absence
// rather than described in a comment. Three ways in, and none of them is pinned by name — a named
// ban only forbids the axis somebody already thought of, and the platform ships axes this repository
// does not know:
//
//   - ANY label under rbacAggregationAxisPrefix. Composed onto an axis, this role hands the whole set
//     — reading subtrees rewritten for another namespace, loading trees back in, moving a volume's
//     bytes — to every holder of the corresponding access level, with nobody binding anything. The
//     cluster-level axis is only the nearest example: this role already carries
//     rbac.deckhouse.io/kind: use, the other half of what the platform's own aggregates select on, so
//     a single line naming any of their axes is enough, and those aggregates are delegatable on top.
//   - ANY annotation, refused exactly as it is on the capabilities this role collects: the access
//     levels of the cluster roles shipped here are assembled from an annotation, and a hook turns one
//     on a role into bindings for the subjects of that level — including a CLUSTER-wide one, which is
//     precisely the boundary "grants nothing until a RoleBinding names it" claims to hold.
//   - rbac.deckhouse.io/delegatable, pinned separately because it is not under that prefix. It would
//     add this role to the set a project owner may hand out inside their own project, while
//     connecting an external backup product to a cluster is not a per-project call.
//
// Empty rules are not an oversight: in the cluster the clusterrole-aggregation-controller fills them
// from the roles the selector matches. A rule written here would be overwritten and would only
// mislead a reader of the chart.
//
// The selector is pinned down to its exact labels so that widening it takes a decision. A second
// selector, an extra matchLabels entry, or a matchExpressions clause is each another set of roles
// whose rules enter this one, and each would pass a check that only looked for the labels expected.
//
// The FILE COUNT of the directory is pinned too, fail-closed, because TestPartnerDocRoleNamesExist
// treats every role published here as one the backup integration document may name: an unpinned file
// added here would be simultaneously unpinned and legal to advertise to a reader.
//
// LIMIT: this describes the chart, and it recognizes a way in by its NAME — a label under that one
// prefix, an annotation, that one label. A future platform mechanism keyed on something else would
// pass. Nor does any of it reach the cluster: a label added to the live object is invisible here, and
// whether the aggregation controller filled this role at all — and with which rules — is past what a
// guard over a template can answer. NOTHING in this repository answers it today: doing so takes a
// subject holding this role and no other, and an assertion on the rules the role ends up carrying.
func TestBackupAgentAggregateRoleCollectsOneAxisAndSpreadsThroughNone(t *testing.T) {
	files := templateYAMLFiles(t, aggregateRolesDir(t))
	if len(files) != 1 {
		t.Fatalf("templates/rbacv2/use/roles holds %d file(s), want exactly the one pinned here: %v — a role added there is unpinned, and being published from this directory it may already be named to a reader of the backup integration document",
			len(files), files)
	}
	file := files[0]

	roles := clusterRolesFromTemplate(t, file)
	if len(roles) != 1 {
		t.Fatalf("%s: holds %d ClusterRole(s), want exactly one", file, len(roles))
	}
	role := roles[0]

	if role.Name != backupAgentRoleName {
		t.Errorf("%s: role is named %q, want %q — the name is what a reader of the document binds", file, role.Name, backupAgentRoleName)
	}

	requiredLabels := map[string]string{
		"heritage":               "deckhouse",
		"module":                 "state-snapshotter",
		"rbac.deckhouse.io/kind": "use",
	}
	for key, value := range requiredLabels {
		if got := role.Labels[key]; got != value {
			t.Errorf("%s: label %q is %q, want %q", file, key, got, value)
		}
	}

	// No aggregation axis, whatever it is called. This role is a collector, not a member of anything:
	// an axis on it composes the entire set into somebody else's rights, and which sets exist is
	// decided outside this repository.
	labelsScanned := 0
	for key, value := range role.Labels {
		labelsScanned++
		if strings.HasPrefix(key, rbacAggregationAxisPrefix) {
			t.Errorf("%s: carries the aggregation axis %s=%q — this role must carry none: it collects the whole permission set of an external backup product, and an axis hands that set to every holder of the corresponding access level at once, with nobody binding anything. The axis to add is on the CAPABILITIES this role collects, never on this role",
				file, key, value)
		}
	}
	if got, labelled := role.Labels["rbac.deckhouse.io/delegatable"]; labelled {
		t.Errorf("%s: carries rbac.deckhouse.io/delegatable=%q, which this role must not have: it would put the role into the set a project owner may hand out inside their own project, and connecting an external backup product to the cluster is not a per-project decision",
			file, got)
	}

	// Annotations are refused outright, the same way and for the same reason as on the capabilities
	// this role collects — with the boundary this role claims to hold making it worse here: an
	// access-level annotation is turned into bindings by a platform hook, cluster-wide ones included,
	// so the whole set would reach subjects nobody bound it to.
	for key, value := range role.Annotations {
		t.Errorf("%s: carries annotation %q=%q — a role that collects capabilities reaches a subject through an explicit binding and nothing else, and an annotation is another way in: the access levels of the cluster roles shipped here are assembled from one, and a hook turns it into bindings for that level's subjects",
			file, key, value)
	}

	if len(role.Rules) != 0 {
		t.Errorf("%s: declares %d rule(s), want none — the rules of this role are filled in the cluster from the roles its selector matches, so a rule written here is overwritten and describes nothing: %v",
			file, len(role.Rules), role.Rules)
	}

	selectorLabelsChecked := 0
	wantSelector := map[string]string{
		rbacBackupAgentLabel:     "member",
		"rbac.deckhouse.io/kind": "use",
	}
	switch {
	case role.AggregationRule == nil:
		t.Errorf("%s: declares no aggregationRule — the role then stays as empty as it is written here and a binding to it grants nothing at all", file)
	case len(role.AggregationRule.ClusterRoleSelectors) != 1:
		t.Errorf("%s: aggregationRule holds %d selector(s), want exactly one — each further selector is an independent set of roles whose rules land in this one",
			file, len(role.AggregationRule.ClusterRoleSelectors))
	default:
		selector := role.AggregationRule.ClusterRoleSelectors[0]
		if len(selector.MatchExpressions) > 0 {
			t.Errorf("%s: the selector carries matchExpressions %v, which the comparison below does not model — an expression can match far more roles than a label equality does",
				file, selector.MatchExpressions)
		}
		selectorLabelsChecked = len(selector.MatchLabels)
		if !reflect.DeepEqual(selector.MatchLabels, wantSelector) {
			t.Errorf("%s: the selector matches %v, want exactly %v — dropping a label widens the set of roles collected here, and adding one drops roles out of it with no error anywhere",
				file, selector.MatchLabels, wantSelector)
		}
	}

	t.Logf("checked the aggregate role %q in %d file(s) under templates/rbacv2/use/roles: %d label(s) required, %d label(s) scanned against the %q prefix and rbac.deckhouse.io/delegatable, %d annotation(s) (all refused), %d rule(s) declared, %d selector label(s) compared",
		role.Name, len(files), len(requiredLabels), labelsScanned, rbacAggregationAxisPrefix, len(role.Annotations), len(role.Rules), selectorLabelsChecked)
	if labelsScanned == 0 || selectorLabelsChecked == 0 {
		t.Fatalf("scanned %d label(s) and compared %d selector label(s) — a zero on either side means the role was not read, not that it is clean",
			labelsScanned, selectorLabelsChecked)
	}
}

// TestBackupAgentAxisIsCarriedByExactlyThePinnedRoles pins the membership of the set an external
// backup product receives in one binding: which roles of this chart carry rbacBackupAgentLabel, and —
// the half that is easy to leave out — that no other role of the chart carries it. One line of YAML is
// all it takes to add a role's rules to what the aggregate hands out, and templates/rbacv2/use/view.yaml
// already carries the selector's other label, rbac.deckhouse.io/kind: use, so that line on that file
// would pour its grants into the set with nothing else to notice.
//
// SCOPE — every template under templates/rbacv2, deliberately the whole tree and not just the
// directory the set lives in: the hole this closes is the label on a role nobody expects in the set,
// and a scan of that one directory could not see it. That subtree is also exactly as far as reading
// templates as objects goes — not one file in it contains a Helm action, so the labels parsed out of
// it are the labels the cluster gets.
//
// The chart's other ClusterRoles — templates/rbac-for-us.yaml, templates/controller/rbac-for-us.yaml,
// templates/webhooks/rbac-for-us.yaml and templates/user-authz-cluster-roles.yaml — are out of scope,
// and not because scanning them would prove nothing: they cannot be scanned here at all. Their labels
// arrive from a Helm action, and the flat parse this guard shares with the rest of this file stops on
// that line with "error converting YAML to JSON" — not one document of those four parses — so pointing
// the walk at them would fail the test on the parse rather than check anything. What
// templates/user-authz-cluster-roles.yaml grants to cluster-level roles is deliberate and
// long-standing, which TestUseRolesDoNotGrantClusterScopedSnapshotLayer says where it draws its own
// scope; that is a separate question from the axis tracked here.
//
// LIMIT, and the blind spot worth watching is this rather than those four files being skipped: ANY
// role outside templates/rbacv2 that carries both selector labels joins the set unseen here — a
// static one because nothing walks it, a templated one because nothing here can read it. Both labels
// on a role in templates/rbac-for-us.yaml would pour its rules into the set an external backup
// product is bound to with this entire package still green. A role of this chart meant to be a member
// therefore belongs under templates/rbacv2; putting one anywhere else means widening the walk above in
// the same change, and for a templated file that is not a widening but a guard of another kind.
//
// SECOND LIMIT: this sees the templates of this chart, while the aggregate collects any ClusterRole in
// the CLUSTER carrying both selector labels. The capability that completes the set — moving a volume's
// bytes, named in partnerDocForeignRole — ships with another module, so that it carries the axis is
// held by a guard in that module's repository and not by anything here.
func TestBackupAgentAxisIsCarriedByExactlyThePinnedRoles(t *testing.T) {
	want := map[string]string{
		capabilityRolePrefix + "view_snapshots":     "member",
		capabilityRolePrefix + "manage_snapshots":   "member",
		capabilityRolePrefix + "download_snapshots": "member",
		capabilityRolePrefix + "upload_snapshots":   "member",
		capabilityRolePrefix + "restore_snapshots":  "member",
	}

	rbacDir := filepath.Join(filepath.Clean("../../../../.."), "templates", "rbacv2")
	files := templateYAMLFiles(t, rbacDir)

	got := map[string]string{}
	rolesScanned := 0
	for _, file := range files {
		for _, role := range clusterRolesFromTemplate(t, file) {
			rolesScanned++
			value, labelled := role.Labels[rbacBackupAgentLabel]
			if !labelled {
				continue
			}
			if _, pinned := want[role.Name]; !pinned {
				t.Errorf("%s: role %q carries %s=%q and is not one of the %d pinned members — its rules would be handed out with the set, so either it belongs there and belongs on the list in this test, or the label on it is the defect",
					file, role.Name, rbacBackupAgentLabel, value, len(want))
				continue
			}
			got[role.Name] = value
		}
	}

	for name, value := range want {
		switch gotValue, member := got[name]; {
		case !member:
			t.Errorf("%s does not carry %s — the role then stays out of the set an external backup product is bound to, and the capability it grants is missing from that binding with no error at binding time",
				name, rbacBackupAgentLabel)
		case gotValue != value:
			t.Errorf("%s carries %s=%q, want %q — the selector matches on the value, so any other value leaves the role out of the set", name, rbacBackupAgentLabel, gotValue, value)
		}
	}

	t.Logf("scanned %d file(s) and %d role(s) under templates/rbacv2 for %s: %d of %d pinned member(s) found",
		len(files), rolesScanned, rbacBackupAgentLabel, len(got), len(want))
	if rolesScanned == 0 {
		t.Fatal("no role scanned under templates/rbacv2 — zero findings means the walk stopped reading the templates, not that the set is intact")
	}
}

// TestUseRolesDoNotGrantClusterScopedSnapshotLayer keeps the cluster-scoped layer of this module out
// of the roles handed to users. A client reads and writes namespaced objects and namespaced
// subresources; the snapshot contents, the manifest checkpoints and their chunks, and the snapshot
// definitions are the module's internals, and raw chunk data is the most sensitive of them.
//
// The prefixes are SINGULAR on purpose: the chunk resource is named manifestcheckpointcontentchunks,
// which the plural "manifestcheckpoints" does not cover — the exact hole this test exists to close.
// A prefix also covers every subresource of the resource (snapshotcontents/manifests-download,
// manifestcheckpoints/manifests). No namespaced resource of this module starts with one of them.
//
// SCOPE — templates/rbacv2/use/** only, and widening it is NOT the fix if it ever fails elsewhere:
// templates/user-authz-cluster-roles.yaml and templates/rbacv2/manage/** grant manifestcheckpoints
// and snapshotcontents to cluster-level roles deliberately and have done so for a long time; taking
// those grants away would silently remove access somebody depends on.
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
	for _, file := range files {
		for _, role := range clusterRolesFromTemplate(t, file) {
			rolesScanned++
			for _, rule := range role.Rules {
				for _, group := range rule.APIGroups {
					if strings.Contains(group, "*") {
						t.Errorf("%s: role %q grants %v on API group %q — a wildcard group covers this module's cluster-scoped layer too, so it is refused here whatever the resources say",
							file, role.Name, rule.Verbs, group)
					}
				}
				for _, resource := range rule.Resources {
					resourcesScanned++
					if strings.Contains(resource, "*") {
						t.Errorf("%s: role %q grants %v on %q — a wildcard resource (\"*\" or \"*/<subresource>\") reaches this module's cluster-scoped layer, and no prefix comparison can see through it",
							file, role.Name, rule.Verbs, resource)
						continue
					}
					for _, prefix := range clusterScopedPrefixes {
						if strings.HasPrefix(resource, prefix) {
							t.Errorf("%s: role %q grants %v on %q, which belongs to this module's cluster-scoped layer — user roles address namespaced objects and namespaced subresources only",
								file, role.Name, rule.Verbs, resource)
						}
					}
				}
			}
		}
	}

	t.Logf("scanned %d file(s), %d role(s) and %d resource grant(s) under templates/rbacv2/use against %d forbidden prefix(es), wildcards refused",
		len(files), rolesScanned, resourcesScanned, len(clusterScopedPrefixes))
	if resourcesScanned == 0 {
		t.Fatal("no resource grant scanned under templates/rbacv2/use — zero findings means the scan stopped reading the roles, not that they are clean")
	}
}

// TestEveryNamespacedSubresourceRouteHasCapabilityRole is the guard against the drift "an endpoint was
// added and the rights for it were not": every namespaced aggregated subresource this module serves
// must be reachable with the capability roles it publishes, with the verb the endpoint is authorized
// under (the aggregated API server authorizes each request with a SubjectAccessReview built from the
// URL, so GET reads as "get" and POST as "create").
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
	granted := map[string]map[string]bool{}
	capabilityFiles := templateYAMLFiles(t, capabilityRolesDir(t))
	for _, file := range capabilityFiles {
		for _, role := range clusterRolesFromTemplate(t, file) {
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

	var namespacedChecked, verbsChecked, clusterScopedSkipped int
	for _, list := range servedAggregatedResources(t) {
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
					t.Errorf("%s serves %q for verb %q, which no capability role grants — add it to a role under templates/rbacv2/use/capabilities (a served endpoint nobody can be granted is unusable)",
						list.GroupVersion, resource.Name, verb)
				}
			}
		}
	}

	t.Logf("checked %d namespaced subresource(s) and %d verb(s) advertised by the aggregated discovery documents against %d capability role file(s); skipped %d cluster-scoped entry(ies)",
		namespacedChecked, verbsChecked, len(capabilityFiles), clusterScopedSkipped)
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

	log, err := logger.NewLogger("error")
	if err != nil {
		t.Fatalf("create logger: %v", err)
	}

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
	return filepath.Join(filepath.Clean("../../../../.."), "templates", "rbacv2", "use", "capabilities")
}

// aggregateRolesDir holds the roles that COLLECT capabilities instead of granting them. It is a
// helper of its own and not a second entry in capabilityRolesDir: that one feeds the guards which
// count and pin capabilities, and a collector is not a capability — folding the two would break those
// counts and pull the collector into a coverage comparison it has no place in.
func aggregateRolesDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Clean("../../../../.."), "templates", "rbacv2", "use", "roles")
}

// clusterRolesFromTemplate parses every document of an RBAC template file. The templates under
// templates/rbacv2/use are plain YAML with no Helm actions in them, which is what lets them be read as
// objects here.
//
// A document that is not a ClusterRole fails the read instead of being skipped, and that is the whole
// point of the check: the guards above count roles and files, so a second document appended to a file
// they already pin — a ClusterRoleBinding handing a capability to every authenticated subject, say —
// would otherwise pass through all of them unseen. Something else genuinely belonging here means
// teaching these guards about it, not widening this parse.
func clusterRolesFromTemplate(t *testing.T, path string) []rbacv1.ClusterRole {
	t.Helper()

	var roles []rbacv1.ClusterRole
	for i, doc := range strings.Split(readTemplate(t, path), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if role.Kind != "ClusterRole" {
			t.Fatalf("%s: document %d is a %q named %q, and these RBAC templates hold ClusterRoles only — a document of any other kind is invisible to the guards that count roles and files here",
				path, i+1, role.Kind, role.Name)
		}
		roles = append(roles, role)
	}
	return roles
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
