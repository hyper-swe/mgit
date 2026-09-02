//go:build !unix

package worktreesync

// oNoFollow has no portable equivalent off unix; the Lstat check alone
// decides there. No sandbox backend runs on such a host in v1, so the delete
// path this guards never executes on one. Refs: MGIT-168
const oNoFollow = 0
