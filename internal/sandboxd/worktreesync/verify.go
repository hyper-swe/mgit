package worktreesync

import (
	"fmt"
	"sort"
	"strings"
)

// maxNamedUndelivered bounds how many paths a failure names before counting
// the rest, so the refusal stays readable.
const maxNamedUndelivered = 10

// VerifyDelivery reads the applied plan back and refuses a sync that did not
// actually land in the guest's tree.
//
// THE FAILURE THIS EXISTS FOR. Sync reported success while the guest tree
// lacked a created file, and mgit had no idea: the drop was caught by a
// consumer's stale-copy check ONE LAYER UP (MGIT-164). A substrate that
// reports work it did not do is worse than one that fails, because an agent
// then builds against a tree missing its own changes and everything derived
// from it inherits the error silently.
//
// It does not depend on knowing WHY an operation was dropped, which is the
// point: the root cause of that instance is still unreproduced, and this turns
// the whole CLASS from silent into loud regardless. The report is about what
// the guest can now READ, not about what the host intended.
//
// Only planned paths are checked. A guest is entitled to its own files, and
// policing paths outside the plan would refuse syncs over work the guest
// legitimately created. Refs: MGIT-164, MGIT-76
func VerifyDelivery(plan Plan, intended, actual Manifest) error {
	var missing []string

	for _, path := range plan.Update {
		want, planned := intended[path]
		if !planned {
			// The host no longer has it; the apply had nothing to write. Not a
			// delivery failure, and not this check's business.
			continue
		}
		got, present := actual[path]
		if !present {
			missing = append(missing, path+" (absent)")
			continue
		}
		if got.Hash != want.Hash {
			missing = append(missing, path+" (stale content)")
			continue
		}
		if got.Mode != want.Mode {
			// An executable bit is a real edit — the same reason Entry carries
			// the mode at all.
			missing = append(missing, fmt.Sprintf("%s (mode %v, expected %v)", path, got.Mode, want.Mode))
		}
	}
	for _, path := range plan.Delete {
		if _, present := actual[path]; present {
			missing = append(missing, path+" (still present)")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &UndeliveredError{Paths: missing}
}

// UndeliveredError reports a sync whose plan did not reach the guest.
type UndeliveredError struct {
	Paths []string
}

// Error names what the guest cannot read, which is the fact a caller needs —
// not what the host meant to send.
func (e *UndeliveredError) Error() string {
	shown := e.Paths
	more := ""
	if len(shown) > maxNamedUndelivered {
		more = fmt.Sprintf(" (and %d more)", len(shown)-maxNamedUndelivered)
		shown = shown[:maxNamedUndelivered]
	}
	return fmt.Sprintf(
		"worktree sync: the guest's tree does not contain what was just synced into it: %s%s. "+
			"Nothing is reported as delivered, because it was not. The host worktree is unchanged "+
			"and safe; re-run the sync, and if it fails the same way the guest's tree is not the "+
			"one being written to",
		strings.Join(shown, ", "), more)
}
