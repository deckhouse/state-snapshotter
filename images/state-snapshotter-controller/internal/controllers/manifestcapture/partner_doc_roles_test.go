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

// The subject here is a document, not a template: it checks that the capability roles the backup
// integration document tells its reader to bind are roles that exist. It lives next to the RBAC
// template guards and reuses their helpers (the published roles are read from the same templates), but
// keeps its own file because what it can fail on — a role renamed on either side of the sentence — is
// a documentation defect, not an RBAC one.
//
// It is a plain testing guard reading a repository file from disk, for the same reason the document
// guards in internal/api are: the subject of the check IS the file, so an embedded copy would be a copy
// checking itself.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// partnerDocFiles are the two files of the backup integration document — the contract an external
// backup product implements against. Its permissions section tells a reader which roles to bind, so a
// role name that is wrong there grants nothing and fails at a moment nobody connects to a typo.
var partnerDocFiles = []string{"BACKUP_INTEGRATION.md", "BACKUP_INTEGRATION_RU.md"}

// partnerDocForeignRole is the ONE role of another module that document may name: the volume-data
// transfer capability, without which a client can neither read a volume's bytes nor write them back.
// It ships with a different module, so nothing in this repository can confirm it exists — which is
// exactly why the name is pinned here as a literal. A rename on either side then fails a test instead
// of reaching a user whose binding silently grants nothing, and the live side of the contract is
// exercised by the vendor-path end-to-end scenario, which binds this role and moves real bytes.
const partnerDocForeignRole = "d8:use:capability:module:storage-foundation:transfer_volume_data"

// partnerDocRoleNameRE matches a capability role name of ANY module, not just this one's prefix: a
// third module's role appearing in the document is the case that must surface, and a pattern anchored
// on this module's prefix would skip it silently.
var partnerDocRoleNameRE = regexp.MustCompile(`d8:use:capability:module:[a-z0-9-]+:[a-z0-9_]+`)

// TestPartnerDocRoleNamesExist checks every capability role the backup integration document names.
// Names of this module must be published by the chart, from either of two directories the document has
// reason to name: templates/rbacv2/use/capabilities, where one role grants one capability, and
// templates/rbacv2/use/roles, where one role collects several so that a reader binds once instead of
// six times. The single foreign name must be the pinned one; anything else fails. The roles are read
// from the templates, so renaming one without updating the document is caught here rather than by the
// reader of the document.
//
// The two directories are listed HERE and not folded into capabilityRolesDir, which feeds the guards
// that count and pin capabilities: a collecting role is not a capability, and widening that helper
// would break their counts and drag the collector into a coverage comparison it has no place in. What
// keeps the second directory from becoming a way to advertise an unpinned role is that its file count
// is pinned by TestBackupAgentAggregateRoleCollectsOneAxisAndSpreadsThroughNone.
//
// LIMIT: this checks that a named role EXISTS, not that the set named in the document is sufficient for
// the operations it describes. What each role grants is pinned by
// TestUserCapabilityRolesGrantExactRulesAndLabels, and that the set is enough to run the whole client
// path is answered by the end-to-end scenario, not by a guard over text.
func TestPartnerDocRoleNamesExist(t *testing.T) {
	published := map[string]bool{}
	for _, dir := range []string{capabilityRolesDir(t), aggregateRolesDir(t)} {
		for _, file := range templateYAMLFiles(t, dir) {
			for _, role := range clusterRolesFromTemplate(t, file) {
				published[role.Name] = true
			}
		}
	}
	if len(published) == 0 {
		t.Fatal("no role read from templates/rbacv2/use/capabilities or templates/rbacv2/use/roles — with an empty set every documented name would be reported as missing, or none at all")
	}

	repoRoot := filepath.Clean("../../../../..")
	var ownChecked, foreignChecked int
	for _, name := range partnerDocFiles {
		path := filepath.Join(repoRoot, "docs", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v — the document that tells a client which roles to bind is not where this guard looks for it", path, err)
		}
		for _, roleName := range partnerDocRoleNameRE.FindAllString(string(data), -1) {
			switch {
			case strings.HasPrefix(roleName, capabilityRolePrefix):
				ownChecked++
				if !published[roleName] {
					t.Errorf("%s names role %q, which this module does not publish (published: %s)",
						name, roleName, sortedRoleNames(published))
				}
			case roleName == partnerDocForeignRole:
				foreignChecked++
			default:
				t.Errorf("%s names capability role %q, which is neither published by this module nor the one foreign role the document may name (%q). A role of a third module cannot be verified from this repository, so it does not belong in the document without a decision to pin it here too",
					name, roleName, partnerDocForeignRole)
			}
		}
	}

	t.Logf("checked %d role mention(s) of this module and %d of the pinned foreign role across %d document file(s), against %d published role(s)",
		ownChecked, foreignChecked, len(partnerDocFiles), len(published))
	if ownChecked == 0 {
		t.Fatal("the backup integration document names no role of this module — zero mentions means the extractor stopped matching, not that the document is correct")
	}
	if foreignChecked == 0 {
		t.Fatalf("the backup integration document names no volume-data transfer role — without %q the permission set it describes cannot move a volume's bytes, and the client path it documents is impossible", partnerDocForeignRole)
	}
}

func sortedRoleNames(roles map[string]bool) string {
	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
