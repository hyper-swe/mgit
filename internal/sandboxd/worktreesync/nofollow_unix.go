//go:build unix

package worktreesync

import "syscall"

// oNoFollow makes an open refuse a symlink as its final component (ELOOP)
// instead of following it. It is what turns a Lstat answer into a binding one
// against a tree another party can modify concurrently. Refs: MGIT-168
const oNoFollow = syscall.O_NOFOLLOW
