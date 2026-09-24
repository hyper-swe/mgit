package service

import (
	"regexp"
	"strings"
)

// leadingTaskTag is mgit's on-object task tag, `[MGIT:<task>] `, as
// CreateCommit writes it and the store parses it back (store/git
// taskIDFromMessage). Refs: FR-2, MGIT-228
var leadingTaskTag = regexp.MustCompile(`^\[MGIT:[^\]]+\] ?`)

// cherryPickPrefix opens a cherry-pick's message, which copies its source's
// message (tag included) after it: `[MGIT:<b>] cherry-pick <id>: [MGIT:<a>] …`.
// A cherry-pick of a cherry-pick nests the shape, one level per pick.
var cherryPickPrefix = regexp.MustCompile(`^cherry-pick [0-9a-f]+: `)

// withoutTaskTag returns a commit message without mgit's own task tag.
//
// The tag is mgit's bookkeeping inside its store, and the store keeps it. A
// message that leaves the store toward the user's git carries what was
// written, not the tool's name: an adopter whose history must use product
// terms only would otherwise record `[MGIT:…]` through `git am`. Only the tag
// mgit put there is removed. The same characters elsewhere in a message are
// the author's and stay. Refs: MGIT-228
func withoutTaskTag(message string) string {
	var kept strings.Builder
	rest := leadingTaskTag.ReplaceAllString(message, "")
	// Walk the cherry-pick chain: after each `cherry-pick <id>: ` comes the
	// copied source message, whose own leading tag mgit wrote too.
	for {
		loc := cherryPickPrefix.FindStringIndex(rest)
		if loc == nil {
			break
		}
		kept.WriteString(rest[:loc[1]])
		rest = leadingTaskTag.ReplaceAllString(rest[loc[1]:], "")
	}
	kept.WriteString(rest)
	return kept.String()
}
