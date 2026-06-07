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

package snapshotcontent

import (
	"fmt"
	"regexp"
	"strings"
)

// maxPendingListItems caps the number of pending element names rendered in a progress message.
const maxPendingListItems = 5

// formatReadyProgress renders a "<ready>/<total> ready" progress fragment plus a capped pending list:
//
//	"2/5 ready; pending: pvc-a, pvc-b"
//	"5/9 ready; pending: child-a, child-b, child-c, child-d, child-e (+3 more)"
//
// It does not include the leading "waiting for ..." clause; callers prepend the leg-specific prefix.
func formatReadyProgress(ready, total int, pending []string) string {
	out := fmt.Sprintf("%d/%d ready", ready, total)
	if len(pending) == 0 {
		return out
	}
	shown := pending
	extra := 0
	if len(shown) > maxPendingListItems {
		extra = len(shown) - maxPendingListItems
		shown = shown[:maxPendingListItems]
	}
	out += "; pending: " + strings.Join(shown, ", ")
	if extra > 0 {
		out += fmt.Sprintf(" (+%d more)", extra)
	}
	return out
}

// childrenFailedMessageRE parses the canonical ChildrenFailed message produced by
// buildChildrenFailedMessage. leaf and reason are single tokens (no spaces); message is the rest.
var childrenFailedMessageRE = regexp.MustCompile(`^child SnapshotContent \S+ failed: leaf=(\S+) reason=(\S+) message=(.*)$`)

// buildChildrenFailedMessage renders the canonical, parseable terminal ChildrenFailed message that pins
// the original failed leaf (leaf/reason/message) while naming the parent's direct child:
//
//	child SnapshotContent <direct-child> failed: leaf=<failed-leaf> reason=<original-reason> message=<original-message>
func buildChildrenFailedMessage(directChild, leaf, reason, message string) string {
	return fmt.Sprintf("child SnapshotContent %s failed: leaf=%s reason=%s message=%s", directChild, leaf, reason, message)
}

// parseChildrenFailedLeaf extracts the original leaf/reason/message from a child's ChildrenFailed message.
// ok is false when the message is not in the canonical form (then the caller treats the child itself as
// the failed leaf).
func parseChildrenFailedLeaf(message string) (leaf, reason, leafMessage string, ok bool) {
	m := childrenFailedMessageRE.FindStringSubmatch(message)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}
