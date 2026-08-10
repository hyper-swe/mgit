//go:build !unix

package artifactexport

import "io/fs"

// noFollowFlag has no portable equivalent outside unix; the plan-to-copy
// symlink race cannot be closed at open() time here.
const noFollowFlag = 0

// linkIdentity cannot report inode and link count on this platform, so
// hardlink containment is unverifiable and the export is refused (fail
// closed). The sandbox backends artifact export serves are unix-only in v1
// (Windows runs core mgit without a sandbox), so this refuses nothing that
// would otherwise work.
func linkIdentity(fs.FileInfo) (id, links uint64, ok bool) { return 0, 0, false }
