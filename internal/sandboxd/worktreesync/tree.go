package worktreesync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hyper-swe/mgit/internal/model"
)

// ErrConflict is returned when a blocked plan is handed to Apply. The
// all-or-nothing guarantee is enforced here rather than left to the caller's
// discipline.
//
// It IS model.ErrWorktreeSyncConflict, not a copy: the refusal crosses the
// manager, the service, the control plane and the CLI, and every one of those
// layers must be able to classify it as a conflict rather than parse its text.
// Refs: MGIT-71, MGIT-76
var ErrConflict = model.ErrWorktreeSyncConflict

// ErrUnsafePath is returned when a plan names a path that would resolve
// outside the tree being written. Refs: SEC-03
var ErrUnsafePath = errors.New("worktree sync path escapes the tree")

// BuildManifest hashes every regular file in a tree, keyed by its path
// relative to root.
//
// The private store is skipped: it is the guest's own, and sync never touches
// it (see storePrefix). A missing tree yields an empty manifest rather than an
// error — a sandbox that has not been synced yet is an ordinary case.
// Symlinks are recorded by their target rather than followed, so a link and
// the file it points at are distinguishable and no link is traversed here.
// Refs: MGIT-71, SEC-03
func BuildManifest(root string) (Manifest, error) {
	out := Manifest{}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return out, nil
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() {
			if skipPath(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if skipPath(rel) {
			return nil
		}
		entry, err := entryFor(path, d)
		if err != nil {
			return err
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("worktree sync: read tree %s: %w", root, err)
	}
	return out, nil
}

// entryFor hashes one entry. A symlink is hashed by its TARGET TEXT, never by
// following it: following would both leave the tree and conflate a link with
// its contents.
func entryFor(path string, d os.DirEntry) (Entry, error) {
	info, err := d.Info()
	if err != nil {
		return Entry{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return Entry{}, err
		}
		sum := sha256.Sum256([]byte("symlink:" + target))
		return Entry{Hash: hex.EncodeToString(sum[:]), Mode: info.Mode()}, nil
	}
	hash, err := hashFile(path)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Hash: hash, Mode: info.Mode().Perm()}, nil
}

// hashFile returns a file's SHA-256, streamed so a large file costs bounded
// memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from walking a host-owned tree
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Apply realizes a plan: it copies the named paths from the host's freshly
// staged tree into the guest's tree and removes the named deletions.
//
// It refuses a BLOCKED plan outright, so the all-or-nothing guarantee cannot
// be lost by a caller that forgets to check. It touches only what the plan
// names — never the whole tree — which is what leaves guest-created paths
// (node_modules, build caches) intact.
//
// Files are written to a temporary name and renamed into place, so a reader
// sees the old content or the new one, never a partial write. Refs: MGIT-71
func Apply(stagedTree, guestTree string, plan Plan) error {
	if plan.Blocked() {
		return fmt.Errorf("%w: %d path(s)", ErrConflict, len(plan.Conflicts))
	}
	for _, rel := range plan.Update {
		src, err := safeJoin(stagedTree, rel)
		if err != nil {
			return err
		}
		dst, err := safeJoin(guestTree, rel)
		if err != nil {
			return err
		}
		if err := copyInto(src, dst); err != nil {
			return fmt.Errorf("worktree sync: deliver %s: %w", rel, err)
		}
	}
	for _, rel := range plan.Delete {
		dst, err := safeJoin(guestTree, rel)
		if err != nil {
			return err
		}
		if err := removeForGuest(dst); err != nil {
			return fmt.Errorf("worktree sync: remove %s: %w", rel, err)
		}
	}
	return nil
}

// removeForGuest deletes a delivered path so that a RUNNING guest cannot read
// its old contents, even where the guest's view of the name outlives the
// unlink.
//
// WHY IT TRUNCATES FIRST, measured on real KVM (MGIT-90). The share is a host
// directory the guest has mounted, and the guest's kernel caches name lookups
// for as long as the filesystem's entry timeout. On libkrun's Linux virtio-fs
// that timeout is ~5 SECONDS: after a plain unlink the guest's directory
// listing is correct immediately, but a process that had already looked the
// name up keeps resolving it — and reads THE OLD CONTENT — for those seconds.
// The verb meanwhile reported the delete as applied. That is the
// silent-staleness failure ADR-011 exists to prevent: an agent that removes a
// file and re-runs its tests would test the removed file and believe the
// result. (On macOS/libkrun the same measurement shows a 0.00s timeout, which
// is why this never surfaced there.)
//
// Emptying the file before unlinking closes the dangerous half. Truncation IS
// observed immediately — measured on both platforms — so a guest holding the
// stale name finds an EMPTY file rather than the old bytes: a build that reads
// it fails loudly instead of silently succeeding against deleted code. The
// name itself may linger briefly; that residual is documented rather than
// papered over.
//
// It is unconditional because it costs one syscall and is correct everywhere:
// on a backend with no entry cache the truncate is simply invisible.
// Refs: MGIT-90, MGIT-76, ADR-011
func removeForGuest(path string) error {
	if err := os.Truncate(path, 0); err != nil && !os.IsNotExist(err) {
		// A directory, or a path this process cannot truncate, is not a
		// failure of the delete — the unlink below is what has to succeed.
		if !errors.Is(err, syscall.EISDIR) {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// safeJoin resolves a tree-relative path, refusing anything that would land
// outside the tree. Plans are host-derived, so this is defense in depth
// against a future caller composing one from less trustworthy input.
func safeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrUnsafePath, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q traverses out", ErrUnsafePath, rel)
	}
	return filepath.Join(root, clean), nil
}

// copyInto writes src to dst IN PLACE, creating parent directories.
//
// WHY NOT WRITE-THEN-RENAME, the usual atomic idiom: the destination here is a
// virtiofs share the GUEST has mounted, and a rename swaps in a new inode
// underneath the guest's cached dentry. Measured on a real libkrun VM, a
// host-side rename made the file read as ENOENT inside the guest and stayed
// that way — the host tree was verifiably correct while the guest saw no file
// at all. Truncating and rewriting keeps the inode, so the guest's cached
// dentry stays valid and it observes the new content.
//
// The ordering guarantee rename would have given comes from the sandbox's sync
// lock instead: no exec runs while a sync is applying, so no command observes a
// partially written tree.
//
// A SYMLINK IS DELIVERED AS A SYMLINK (MGIT-165). staging preserves an
// in-tree link and BuildManifest records it by its target text; this used to
// Stat and Open the source, both of which FOLLOW the link, and so wrote a
// regular file holding the target's bytes into the guest. The MGIT-164
// read-back then correctly called that stale content and refused the whole
// sync — every worktree with an ordinary internal link (a monorepo package
// link, docs/x.md -> ../x.md) launched fine and could never be synced. The
// producer and the verifier now agree: the link is recreated with the same
// target text, which is exactly what the manifest hashes.
//
// AND NOTHING IS EVER WRITTEN THROUGH A LINK. O_TRUNC follows links too, so
// an update landing on a path where the guest holds a link would have emptied
// and overwritten the link's TARGET — resolved on the host, by the daemon,
// wherever the guest chose to point it. The destination is cleared first
// unless it is a regular file, and the open carries O_NOFOLLOW so a swap in
// the window between the two is refused rather than followed. A link that is
// replaced gets a new inode, so a guest holding its old dentry sees ENOENT
// for the entry timeout (~5s on KVM virtio-fs, 0s on macOS) — the same
// residual removeForGuest documents, and for a link it is the right one:
// there is no content to keep stable, only a name. Refs: MGIT-71, MGIT-165,
// SEC-03, ADR-011
func copyInto(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return deliverLink(src, dst)
	}
	if err := clearNonRegular(dst); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // src is inside the host-owned staged tree
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|oNoFollow, info.Mode().Perm()) //nolint:gosec // dst is inside the sandbox's own staged tree
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Chmod explicitly: O_CREATE's mode applies only to a NEW file, so an
	// existing file would keep its old permissions.
	return os.Chmod(dst, info.Mode().Perm())
}

// deliverLink recreates the link at src as a link at dst, carrying the same
// target text so the read-back's hash of "symlink:"+target matches. Whatever
// the guest holds at dst is removed first; a link is never written through
// and never opened. Refs: MGIT-165, SEC-03
func deliverLink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(target, dst)
}

// clearNonRegular removes whatever sits at dst unless it is a regular file,
// so the write that follows lands on a fresh inode rather than THROUGH a
// guest-planted link, and a regular file keeps its inode for the guest's
// cached dentry. A non-empty directory at dst fails here, loudly: the host
// turned a directory into a file while the guest filled the directory, and
// that is a conflict for a human, not something to delete. Refs: MGIT-165
func clearNonRegular(dst string) error {
	info, err := os.Lstat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode().IsRegular() {
		return nil
	}
	return os.Remove(dst)
}
