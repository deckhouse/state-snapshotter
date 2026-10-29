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
var partnerDocFiles = []string{"BACKUP_INTEGRATION.md", "BACKUP_INTEGRATION.ru.md"}

// partnerDocForeignRoles is the ONE role of another module that document may name, under the name of
// each model: the volume-data transfer capability, without which a client can neither read a volume's
// bytes nor write them back. It ships with a different module, so nothing in this repository can
// confirm it exists — which is exactly why both names are pinned here as literals. A rename on either
// side then fails a test instead of reaching a user whose binding silently grants nothing, and the live
// side of the contract is exercised by the vendor-path end-to-end scenario, which binds this role and
// moves real bytes.
var partnerDocForeignRoles = map[rbacScheme]string{
	schemeNew:    "d8:namespace-capability:storage-foundation:transfer_volume_data",
	schemeLegacy: "d8:use:capability:module:storage-foundation:transfer_volume_data",
}

// partnerDocRoleNameRE matches a capability role name of ANY module, in either model, not just this
// module's prefixes: a third module's role appearing in the document is the case that must surface,
// and a pattern anchored on this module's prefix would skip it silently. The collecting role of the new
// model is outside the role model and has no capability prefix to match on, so its name is spelled out;
// the legacy one is matched by the capability pattern, as it always was.
var partnerDocRoleNameRE = regexp.MustCompile(`d8:(?:use:capability:module|namespace-capability):[a-z0-9-]+:[a-z0-9_]+|d8:state-snapshotter:backup-agent`)

// TestPartnerDocRoleNamesExist checks every role the backup integration document names, in both
// models: the document gives the names of DKP 1.78 and later as the primary ones and the legacy names
// for clusters below it, and a binding to either kind of name that is wrong grants nothing.
//
// Names of this module must be published by the chart in the model their prefix belongs to, from the
// directories the document has reason to name: templates/rbacv2/use/capabilities, where one role
// grants one capability (both models), templates/backup-agent (the collecting role of the new model)
// and templates/rbacv2/use/roles (the collecting role of the legacy model), where one role collects
// several so that a reader binds once instead of six times. The single foreign role must be one of its
// two pinned names; anything else fails. The roles are read from the templates, so renaming one without
// updating the document is caught here rather than by the reader of the document.
//
// Both FAMILIES must be present, and the foreign role under both names: a document that dropped the
// legacy names would leave every cluster below 1.78 with nothing to bind, and one that dropped the new
// names would teach names that are only an alias there.
//
// The directories are listed HERE and not folded into capabilityRolesDir, which feeds the guards that
// count and pin capabilities: a collecting role is not a capability, and widening that helper would
// break their counts and drag the collector into a coverage comparison it has no place in. What keeps
// the collecting directories from becoming a way to advertise an unpinned role is that their file
// counts are pinned by TestBackupAgentAggregateRoleCollectsOneAxisAndSpreadsThroughNone.
//
// LIMIT: this checks that a named role EXISTS, not that the set named in the document is sufficient for
// the operations it describes. What each role grants is pinned by
// TestUserCapabilityRolesGrantExactRulesAndLabels, and that the set is enough to run the whole client
// path is answered by the end-to-end scenario, not by a guard over text.
func TestPartnerDocRoleNamesExist(t *testing.T) {
	published := map[rbacScheme]map[string]bool{}
	for scheme, dirs := range map[rbacScheme][]string{
		schemeNew:    {capabilityRolesDir(t), backupAgentRoleDir(t)},
		schemeLegacy: {capabilityRolesDir(t), aggregateRolesDir(t)},
	} {
		published[scheme] = map[string]bool{}
		for _, dir := range dirs {
			for _, file := range templateYAMLFiles(t, dir) {
				for _, role := range clusterRolesFromTemplate(t, file, scheme) {
					published[scheme][role.Name] = true
				}
			}
		}
		if len(published[scheme]) == 0 {
			t.Fatalf("no role of the %s model read from %v — with an empty set every documented name would be reported as missing, or none at all", scheme, dirs)
		}
	}

	// schemeOfOwnName is the model a name of this module belongs to, by its spelling.
	schemeOfOwnName := func(roleName string) (rbacScheme, bool) {
		switch {
		case roleName == backupAgentRoleName || strings.HasPrefix(roleName, capabilityRolePrefix):
			return schemeNew, true
		case strings.HasPrefix(roleName, legacyCapabilityRolePrefix):
			return schemeLegacy, true
		}
		return "", false
	}

	repoRoot := filepath.Clean("../../../../..")
	ownChecked := map[rbacScheme]int{}
	foreignChecked := map[rbacScheme]int{}
	for _, name := range partnerDocFiles {
		path := filepath.Join(repoRoot, "docs", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v — the document that tells a client which roles to bind is not where this guard looks for it", path, err)
		}
	mentions:
		for _, roleName := range partnerDocRoleNameRE.FindAllString(string(data), -1) {
			if scheme, own := schemeOfOwnName(roleName); own {
				ownChecked[scheme]++
				if !published[scheme][roleName] {
					t.Errorf("%s names role %q, which this module does not publish in the %s model (published there: %s)",
						name, roleName, scheme, sortedRoleNames(published[scheme]))
				}
				continue
			}
			for scheme, foreign := range partnerDocForeignRoles {
				if roleName == foreign {
					foreignChecked[scheme]++
					continue mentions
				}
			}
			t.Errorf("%s names capability role %q, which is neither published by this module nor the one foreign role the document may name (%v). A role of a third module cannot be verified from this repository, so it does not belong in the document without a decision to pin it here too",
				name, roleName, partnerDocForeignRoles)
		}
	}

	for _, scheme := range rbacSchemes {
		t.Logf("%s: checked %d role mention(s) of this module and %d of the pinned foreign role across %d document file(s), against %d published role(s)",
			scheme, ownChecked[scheme], foreignChecked[scheme], len(partnerDocFiles), len(published[scheme]))
		if ownChecked[scheme] == 0 {
			t.Errorf("the backup integration document names no role of this module in the %s model — either the extractor stopped matching, or the readers on those clusters are left with nothing to bind", scheme)
		}
		if foreignChecked[scheme] == 0 {
			t.Errorf("the backup integration document names no volume-data transfer role of the %s model — without %q the permission set it describes cannot move a volume's bytes there, and the client path it documents is impossible",
				scheme, partnerDocForeignRoles[scheme])
		}
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
