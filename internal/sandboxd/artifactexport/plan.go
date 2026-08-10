package artifactexport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/staging"
)

// entry is one planned export item, resolved relative to the exported
// subtree's root ("" is the root itself, for a single-file export).
type entry struct {
	rel        string
	mode       fs.FileMode
	modeSource string // where mode was observed (ModeSourceHostStat/ShareRecord)
	isDir      bool
	symlink    string // link target text; non-empty only for symlinks
}

// plan is the fully validated description of what an export will write. It is
// built BEFORE any host write, which is what makes "refused" mean "the host
// filesystem was never touched".
type plan struct {
	root    string // absolute source path inside the staged tree
	entries []entry
}

// linkRef accumulates what the planner saw of one inode inside the subtree, so
// a file whose links reach outside can be refused.
type linkRef struct {
	seen  int
	links uint64
	path  string
}

// buildPlan validates the request and enumerates exactly what would be
// written. Every containment rule is enforced here: path shape, subtree
// containment, symlink and hardlink escapes, irregular files, and the byte and
// file-count ceilings. Refs: MGIT-73, SEC-03
func buildPlan(req Request) (*plan, error) {
	rel, err := safeGuestPath(req.GuestPath)
	if err != nil {
		return nil, err
	}
	root, info, err := resolveSource(req.StagedTree, rel)
	if err != nil {
		return nil, err
	}
	limits := req.Limits.withDefaults()
	switch {
	case info.IsDir():
		return planSubtree(root, limits)
	case info.Mode().IsRegular():
		return planSingleFile(root, info, limits)
	default:
		return nil, fmt.Errorf("%w: %q is not a regular file or directory", ErrUnsafePath, req.GuestPath)
	}
}

// safeGuestPath validates the host-named, worktree-relative source path and
// returns its cleaned form.
//
// The private store is refused outright: committed objects cross the boundary
// only through the verified land airlock, and an export that could copy
// <worktree>/.mgit would be a second, unverified route for them. Refs: SEC-03, ADR-011
func safeGuestPath(guestPath string) (string, error) {
	switch {
	case guestPath == "":
		return "", fmt.Errorf("%w: the guest path must not be empty", ErrUnsafePath)
	case strings.ContainsRune(guestPath, 0):
		return "", fmt.Errorf("%w: the guest path contains a NUL byte", ErrUnsafePath)
	case filepath.IsAbs(guestPath), strings.HasPrefix(guestPath, "/"):
		return "", fmt.Errorf("%w: %q is absolute; name a worktree-relative path", ErrUnsafePath, guestPath)
	}
	clean := filepath.Clean(guestPath)
	if clean == "." {
		return "", fmt.Errorf("%w: refusing to export the whole worktree; "+
			"name a subpath (committed work crosses through `mgit sandbox land`)", ErrUnsafePath)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q traverses out of the worktree", ErrUnsafePath, guestPath)
	}
	if first, _, _ := strings.Cut(clean, string(filepath.Separator)); first == staging.GuestStoreName {
		return "", fmt.Errorf("%w: %q is the sandbox's private mgit store; "+
			"commits cross the boundary only through the verified land path", ErrUnsafePath, guestPath)
	}
	return clean, nil
}

// resolveSource locates the export root inside the staged tree and proves it
// really lies within it. A symlinked source is refused rather than followed:
// the host named a path, not whatever the guest pointed it at.
func resolveSource(stagedTree, rel string) (string, fs.FileInfo, error) {
	if stagedTree == "" {
		return "", nil, fmt.Errorf("%w: no staged worktree to export from", ErrUnsafePath)
	}
	root := filepath.Clean(stagedTree)
	if eval, err := filepath.EvalSymlinks(root); err == nil {
		root = eval
	}
	src := filepath.Join(root, rel)
	info, err := os.Lstat(src)
	if os.IsNotExist(err) {
		return "", nil, fmt.Errorf("%w: %q", ErrSourceNotFound, rel)
	}
	if err != nil {
		return "", nil, fmt.Errorf("artifact export: read %q in the guest worktree: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("%w: %q is a symlink; name the real path to export", ErrUnsafePath, rel)
	}
	// Belt and braces against an intermediate symlinked component: the
	// resolved source must still be inside the staged tree.
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", nil, fmt.Errorf("artifact export: resolve %q: %w", rel, err)
	}
	if !withinRoot(root, resolved) {
		return "", nil, fmt.Errorf("%w: %q resolves outside the guest worktree", ErrUnsafePath, rel)
	}
	return src, info, nil
}

// withinRoot reports whether path is root itself or lies beneath it.
func withinRoot(root, path string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// planSingleFile plans the export of one regular file.
func planSingleFile(root string, info fs.FileInfo, limits Limits) (*plan, error) {
	if info.Size() > limits.MaxBytes {
		return nil, limitError(info.Size(), 1, limits)
	}
	links := map[uint64]*linkRef{}
	if err := accountLink(links, root, info); err != nil {
		return nil, err
	}
	if err := checkHardlinks(links); err != nil {
		return nil, err
	}
	mode, source := observedMode(root, info)
	return &plan{root: root, entries: []entry{{mode: mode, modeSource: source}}}, nil
}

// planSubtree walks the exported directory, validating every entry and
// enforcing the ceilings as it goes. The walk never follows a symlink: links
// are recorded by their target text, exactly as staging does inbound.
func planSubtree(root string, limits Limits) (*plan, error) {
	p := &plan{root: root}
	links := map[uint64]*linkRef{}
	var files int
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		e, size, err := planEntry(root, path, rel, d, links)
		if err != nil {
			return err
		}
		if !e.isDir {
			files++
			total += size
			if files > limits.MaxFiles || total > limits.MaxBytes {
				return limitError(total, files, limits)
			}
		}
		p.entries = append(p.entries, e)
		return nil
	})
	if err != nil {
		return nil, wrapWalkError(root, err)
	}
	if err := checkHardlinks(links); err != nil {
		return nil, err
	}
	return p, nil
}

// planEntry validates one walked entry and returns its plan record plus the
// bytes it contributes. A symlink must resolve INSIDE the exported subtree
// (staging's check, applied to the subtree root): a link that merely stays
// inside the worktree would dangle — or resolve to an unrelated host path —
// once the artifact has moved. Refs: SEC-03, F-A/NEW-2
func planEntry(root, path, rel string, d fs.DirEntry, links map[uint64]*linkRef) (entry, int64, error) {
	switch {
	case d.IsDir():
		return entry{rel: rel, isDir: true, mode: d.Type()}, 0, nil
	case d.Type()&os.ModeSymlink != 0:
		if err := staging.AssertSymlinkWithin(root, path); err != nil {
			return entry{}, 0, err
		}
		target, err := os.Readlink(path)
		if err != nil {
			return entry{}, 0, fmt.Errorf("artifact export: read symlink %s: %w", rel, err)
		}
		return entry{rel: rel, symlink: target, mode: d.Type()}, 0, nil
	case d.Type().IsRegular():
		info, err := d.Info()
		if err != nil {
			return entry{}, 0, err
		}
		if err := accountLink(links, path, info); err != nil {
			return entry{}, 0, err
		}
		mode, source := observedMode(path, info)
		return entry{rel: rel, mode: mode, modeSource: source}, info.Size(), nil
	default:
		return entry{}, 0, fmt.Errorf("%w: %q is a %s, which cannot be exported",
			ErrUnsafePath, rel, d.Type().String())
	}
}

// accountLink records one regular file's link identity so hardlink
// containment can be decided once the whole subtree is known.
//
// A platform that cannot report link identity fails CLOSED: an unverifiable
// hardlink is exactly the case this check exists for. (The sandbox backends
// this serves are unix-only.)
func accountLink(links map[uint64]*linkRef, path string, info fs.FileInfo) error {
	id, count, ok := linkIdentity(info)
	if !ok {
		return fmt.Errorf("%w: hardlink containment cannot be verified on this platform", ErrUnsafePath)
	}
	if count <= 1 {
		return nil
	}
	ref, seen := links[id]
	if !seen {
		ref = &linkRef{links: count, path: path}
		links[id] = ref
	}
	ref.seen++
	return nil
}

// checkHardlinks refuses any exported file whose link count exceeds the number
// of links found INSIDE the subtree: the remaining links are elsewhere, so the
// artifact would alias a file the host never named.
func checkHardlinks(links map[uint64]*linkRef) error {
	for _, ref := range links {
		if uint64(ref.seen) < ref.links {
			return fmt.Errorf("%w: %s has %d link(s), %d of them inside the exported subtree",
				ErrHardlinkEscape, ref.path, ref.links, ref.seen)
		}
	}
	return nil
}

// limitError reports a ceiling breach with the numbers that caused it.
func limitError(total int64, files int, limits Limits) error {
	return fmt.Errorf("%w: %d file(s)/%d bytes exceeds the %d file/%d byte ceiling",
		ErrLimitExceeded, files, total, limits.MaxFiles, limits.MaxBytes)
}

// wrapWalkError keeps a sentinel refusal recognizable through the walk while
// giving an incidental I/O failure context.
func wrapWalkError(root string, err error) error {
	for _, sentinel := range []error{ErrUnsafePath, ErrLimitExceeded, ErrHardlinkEscape, staging.ErrSymlinkEscape} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("artifact export: read the guest worktree at %s: %w", root, err)
}

// materialize writes a validated plan into payloadRoot, re-enforcing the byte
// ceiling against the bytes ACTUALLY read: the plan's sizes were measured
// before the copy, and the guest may have grown a file since.
func materialize(p *plan, payloadRoot string, limits Limits) ([]ManifestEntry, int64, error) {
	out := make([]ManifestEntry, 0, len(p.entries))
	var total int64
	for _, e := range p.entries {
		src := filepath.Join(p.root, e.rel)
		dst := filepath.Join(payloadRoot, e.rel)
		switch {
		case e.isDir:
			if err := os.MkdirAll(dst, 0o750); err != nil {
				return nil, 0, fmt.Errorf("artifact export: create %s: %w", e.rel, err)
			}
		case e.symlink != "":
			if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
				return nil, 0, fmt.Errorf("artifact export: create parent of %s: %w", e.rel, err)
			}
			if err := os.Symlink(e.symlink, dst); err != nil {
				return nil, 0, fmt.Errorf("artifact export: link %s: %w", e.rel, err)
			}
			out = append(out, ManifestEntry{Path: manifestPath(p, e), Symlink: e.symlink})
		default:
			sum, n, err := copyRegular(src, dst, e.mode, limits.MaxBytes-total)
			if err != nil {
				return nil, 0, err
			}
			total += n
			out = append(out, ManifestEntry{
				Path: manifestPath(p, e), Mode: fmt.Sprintf("%04o", e.mode.Perm()),
				ModeSource: recordedSource(e.modeSource), Size: n, SHA256: sum,
			})
		}
	}
	return out, total, nil
}

// recordedSource is the mode provenance the sidecar records. A plain host stat
// is the default and stays implicit (an absent field means it), so the sidecar
// calls out only the case a reader would not otherwise expect. Refs: MGIT-81
func recordedSource(source string) string {
	if source == ModeSourceShareRecord {
		return source
	}
	return ""
}

// manifestPath is the path an entry is recorded under: relative to the
// exported root, or the file's own name for a single-file export.
func manifestPath(p *plan, e entry) string {
	if e.rel == "" {
		return filepath.Base(p.root)
	}
	return e.rel
}

// copyRegular copies one file, hashing it as it goes and refusing to exceed
// the remaining byte budget.
//
// The source is opened O_NOFOLLOW: between planning and copying the guest may
// have replaced a planned regular file with a symlink, and following it would
// read a host path outside the subtree. The destination is opened O_EXCL
// inside the private temporary tree.
//
// The mode is applied by an explicit Chmod rather than left to O_CREATE, whose
// mode argument the kernel masks with the exporting process's umask: under a
// daemon running umask 0077 an observed 0755 would otherwise land as 0700
// while the sidecar recorded 0755. Chmod is not umasked, so the file carries
// exactly the mode that was observed — and only ever a mode that was observed.
// The window between create and chmod is invisible: the file is inside this
// export's private 0700 temporary directory until the whole tree is renamed
// into place. Refs: MGIT-81
func copyRegular(src, dst string, mode fs.FileMode, remaining int64) (string, int64, error) {
	if remaining < 0 {
		return "", 0, fmt.Errorf("%w: %s", ErrLimitExceeded, src)
	}
	in, err := os.OpenFile(src, os.O_RDONLY|noFollowFlag, 0) //nolint:gosec // src is a planned path inside the manager-owned staged tree, opened O_NOFOLLOW
	if err != nil {
		return "", 0, fmt.Errorf("artifact export: open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", 0, fmt.Errorf("artifact export: create parent of %s: %w", dst, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm()) //nolint:gosec // dst is inside this export's private temp dir
	if err != nil {
		return "", 0, fmt.Errorf("artifact export: create %s: %w", dst, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, remaining+1))
	if err != nil {
		_ = out.Close()
		return "", 0, fmt.Errorf("artifact export: copy %s: %w", src, err)
	}
	if err := out.Chmod(mode.Perm()); err != nil {
		_ = out.Close()
		return "", 0, fmt.Errorf("artifact export: set the observed mode on %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return "", 0, fmt.Errorf("artifact export: close %s: %w", dst, err)
	}
	if n > remaining {
		return "", 0, fmt.Errorf("%w: %s grew past the byte ceiling during the export", ErrLimitExceeded, src)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
