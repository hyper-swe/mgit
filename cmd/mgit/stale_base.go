package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// A registration owns its requested image reference, including before lazy
// boot. A worktree path or a cached tree alone cannot tell us that pin.
// Inspect only this repository's existing registry; do not start a daemon or
// rewrite a registration. This runs before signing the replacement lock.
func warnStaleBaseUsers(ctx context.Context, hostRoot, name, nextDigest string, out io.Writer) {
	previous, err := images.LookupEntry(hostRoot, name)
	if err != nil || previous.Digest == nextDigest {
		return // first composition (or the normal registration error), or unchanged bytes
	}
	oldRef, nextRef := name+"@"+previous.Digest, name+"@"+nextDigest
	dbPath := filepath.Join(hostRoot, model.SandboxRegistryFile)
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return // no durable registrations have been recorded for this repository
	} else if err != nil {
		warnUnknownBaseUsers(out, oldRef, nextRef, err)
		return
	}
	registrations, err := index.ReadSandboxRegistrations(ctx, dbPath)
	if err != nil {
		warnUnknownBaseUsers(out, oldRef, nextRef, err)
		return
	}
	for _, reg := range registrations {
		if reg.ImageRef != oldRef || !occupiesBase(reg.Info.State) {
			continue
		}
		_, _ = fmt.Fprintf(out, "warning: before changing the signed base pin from %s to %s, "+
			"task %s (worktree %q) still records the old reference. "+
			"An already running VM is not restarted, but a new boot from that reference will be refused; "+
			"when ready to replace it (removing the old sandbox), run:\nmgit sandbox remove %s && "+
			"mgit sandbox launch --task-id %s --worktree %s --image %s\n",
			oldRef, nextRef, reg.Info.TaskID, reg.Info.WorktreePath,
			staleBaseShellArg(reg.Info.TaskID), staleBaseShellArg(reg.Info.TaskID), staleBaseShellArg(reg.Info.WorktreePath), nextRef)
	}
}

func warnUnknownBaseUsers(out io.Writer, oldRef, nextRef string, err error) {
	_, _ = fmt.Fprintf(out, "warning: cannot determine which recorded sandboxes still use %s before switching to %s: %v; "+
		"a new boot from the old reference will be refused; remove the old sandbox before launching its replacement with --image %s.\n", oldRef, nextRef, err, nextRef)
}

// Paths and persisted task IDs are data, not shell syntax in the printed remedy.
func staleBaseShellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
