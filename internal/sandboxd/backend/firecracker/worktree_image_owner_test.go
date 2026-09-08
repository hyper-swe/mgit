package firecracker

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The worktree image's ROOT directory must be owned by the identity that
// delivers it. `mke2fs -d` copies the owners of the tree's contents, but
// since e2fsprogs 1.43 the filesystem's root directory defaults to
// root:root unless told otherwise — so a guest command running as the
// daemon's uid could write every subdirectory of its worktree and not the
// worktree itself (`touch: owned: Permission denied`, CI's unprivileged
// half, 2026-09-08). Refs: MGIT-151
func TestMke2fsArgs_RootDirectoryIsOwnedByTheDeliveringIdentity(t *testing.T) {
	args := mke2fsArgs("/src", "/img.ext4")
	assert.Contains(t, args, "-E")
	assert.Contains(t, args, fmt.Sprintf("root_owner=%d:%d", os.Getuid(), os.Getgid()))
	assert.Equal(t, "/img.ext4", args[len(args)-1], "the image path stays last")
}
