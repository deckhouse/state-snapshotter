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

// The child-quote verbs. The quote format is shared by the terminal (ChildrenFailed) and the non-terminal
// (ChildrenPending) aggregation messages and differs ONLY in this verb, because the two states differ only
// in that: the direct child either failed for good or is still waiting. The verb is a parameter rather than
// two independent formats so a reader (and the parsers below) sees one shape — and so the pending message
// cannot call a child that has not failed "failed", which would state something untrue on a user-facing
// condition.
const (
	childFailedVerb  = "failed"
	childPendingVerb = "is not ready yet"
)

// childLeafMessageRE builds the parser for one verb of the shared child-quote format, so the pattern cannot
// drift from buildChildLeafMessage per verb. leaf and reason are single tokens (no spaces); message is the
// rest of the line. prefixAnchor differs by state, and that difference is load-bearing:
//   - the terminal quote IS the whole ChildrenFailed message, so it anchors at the start (`^`);
//   - the pending quote is APPENDED after the progress fragment of a ChildrenPending message, so it must be
//     locatable inside the line; it is always the LAST clause, so the trailing capture still yields the
//     quoted leaf's message verbatim.
func childLeafMessageRE(verb, prefixAnchor string) *regexp.Regexp {
	return regexp.MustCompile(prefixAnchor + `child SnapshotContent \S+ ` + regexp.QuoteMeta(verb) +
		`: leaf=(\S+) reason=(\S+) message=(.*)$`)
}

var (
	childrenFailedMessageRE  = childLeafMessageRE(childFailedVerb, `^`)
	childrenPendingMessageRE = childLeafMessageRE(childPendingVerb, ``)
)

// buildChildLeafMessage renders the canonical, parseable child quote that pins the original leaf
// (leaf/reason/message) while naming the parent's direct child:
//
//	child SnapshotContent <direct-child> <verb>: leaf=<leaf> reason=<leaf-reason> message=<leaf-message>
//
// Callers use the state-specific wrappers below rather than passing a verb of their own.
func buildChildLeafMessage(verb, directChild, leaf, reason, message string) string {
	return fmt.Sprintf("child SnapshotContent %s %s: leaf=%s reason=%s message=%s",
		directChild, verb, leaf, reason, message)
}

// buildChildrenFailedMessage renders the terminal ChildrenFailed message:
//
//	child SnapshotContent <direct-child> failed: leaf=<failed-leaf> reason=<original-reason> message=<original-message>
func buildChildrenFailedMessage(directChild, leaf, reason, message string) string {
	return buildChildLeafMessage(childFailedVerb, directChild, leaf, reason, message)
}

// buildChildrenPendingMessage renders the non-terminal counterpart quoted in a ChildrenPending message, so
// the reason a subtree is still waiting is readable on the ancestor instead of only on the leaf that knows
// it. The child named here has NOT failed and may still succeed.
func buildChildrenPendingMessage(directChild, leaf, reason, message string) string {
	return buildChildLeafMessage(childPendingVerb, directChild, leaf, reason, message)
}

// parseChildrenFailedLeaf extracts the original leaf/reason/message from a child's ChildrenFailed message.
// ok is false when the message is not in the canonical form (then the caller treats the child itself as
// the failed leaf).
func parseChildrenFailedLeaf(message string) (leaf, reason, leafMessage string, ok bool) {
	return parseChildLeafQuote(childrenFailedMessageRE, message)
}

// parseChildrenPendingLeaf is the ChildrenPending twin of parseChildrenFailedLeaf: it recovers the pinned
// leaf from an intermediate node's pending message so a grandparent re-quotes the DEEPEST waiting leaf
// instead of nesting one whole message inside the next, which would grow the text with every tree level.
func parseChildrenPendingLeaf(message string) (leaf, reason, leafMessage string, ok bool) {
	return parseChildLeafQuote(childrenPendingMessageRE, message)
}

func parseChildLeafQuote(re *regexp.Regexp, message string) (leaf, reason, leafMessage string, ok bool) {
	m := re.FindStringSubmatch(message)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}
