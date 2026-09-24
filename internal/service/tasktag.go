package service

import "regexp"

// leadingTaskTag is mgit's on-object task tag, `[MGIT:<task>] `, as
// CreateCommit writes it and the store parses it back (store/git
// taskIDFromMessage). Refs: FR-2, MGIT-228
var leadingTaskTag = regexp.MustCompile(`^\[MGIT:[^\]]+\] ?`)

// cherryPickTaskTag is the tag a cherry-pick copies from its source, inside
// its own message: `[MGIT:<b>] cherry-pick <id>: [MGIT:<a>] <message>`.
var cherryPickTaskTag = regexp.MustCompile(`^(cherry-pick [0-9a-f]+: )\[MGIT:[^\]]+\] ?`)

// withoutTaskTag returns a commit message without mgit's own task tag.
//
// The tag is mgit's bookkeeping inside its store, and the store keeps it. A
// message that leaves the store toward the user's git carries what was
// written, not the tool's name: an adopter whose history must use product
// terms only would otherwise record `[MGIT:…]` through `git am`. Only the tag
// mgit put there is removed. The same characters elsewhere in a message are
// the author's and stay. Refs: MGIT-228
func withoutTaskTag(message string) string {
	return cherryPickTaskTag.ReplaceAllString(leadingTaskTag.ReplaceAllString(message, ""), "$1")
}
