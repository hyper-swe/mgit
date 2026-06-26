// Package gitref reads the project's git repository READ-ONLY to learn git's
// truth (the current local HEAD commit id) without ever mutating `.git`.
//
// ADR-008 makes mgit git-authoritative: the `.mgit` base is kept coherent with
// the project's current LOCAL working state. To detect drift cheaply mgit needs
// the local git HEAD commit id. Reading it is NEW (mgit was previously fully
// self-contained and never touched `.git`) and so is deliberately DEFENSIVE:
//
//   - `.git` as a DIRECTORY (the common case);
//   - `.git` as a FILE containing a `gitdir: <path>` pointer (linked git
//     worktrees and submodules);
//   - a symlinked `.git`;
//
// and it FAILS LOUD with a clear, typed error for states it cannot safely read
// (shallow clone, sparse-checkout, git-LFS) rather than crash or mis-read.
//
// It NEVER writes to `.git`. The existing `.git`-never-mutated invariant
// (MGIT-14, ADR-001) is preserved: these are pure reads.
//
// Refs: MGIT-35, ADR-008 (§5,§6), MGIT-14
package gitref

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoGit indicates the project directory has no readable `.git` at all (mgit
// can run over a non-git directory; base-derivation degrades to the .mgit base
// rather than hard-failing). Refs: ADR-008 §6
var ErrNoGit = errors.New("no git repository found")

// ErrUnsupportedGitState indicates `.git` exists but is in a state gitref will
// not silently read (e.g. shallow clone, sparse-checkout, git-LFS pointers),
// because a partial read would corrupt the base. Callers FAIL LOUD. Refs: ADR-008 §6
var ErrUnsupportedGitState = errors.New("unsupported git state")

// ErrDetachedOrUnborn indicates HEAD does not resolve to a concrete commit
// (detached HEAD with no commit, or an unborn branch). Refs: ADR-008
var ErrDetachedOrUnborn = errors.New("git HEAD does not resolve to a commit")

// hexSHA1Len is the length of a full hex SHA-1 git object id.
const hexSHA1Len = 40

// maxSymrefDepth bounds symbolic-ref chasing so a cyclic or pathological symref
// chain fails loud instead of looping forever. Refs: ADR-008 §6
const maxSymrefDepth = 10

// LocalState is the read-only snapshot of the project's git truth that mgit's
// drift gate compares against. It carries the resolved local HEAD commit id and
// the symbolic ref HEAD points at (empty when detached), so callers can record
// a stable, content-based drift signal. Refs: ADR-008 §3
type LocalState struct {
	// HeadCommit is the 40-hex SHA-1 of the commit the local HEAD resolves to.
	HeadCommit string
	// Ref is the symbolic ref HEAD points at (e.g. "refs/heads/main"); empty
	// when HEAD is detached. Recorded for diagnostics only — the base is pinned
	// per task, so a later branch switch never retargets an in-flight task.
	Ref string
}

// ReadLocalState reads, READ-ONLY, the project's current git HEAD at projectRoot
// and returns the resolved commit id. It resolves `.git`-as-dir, `.git`-as-file
// (gitdir pointer), and symlinked `.git`. It returns ErrNoGit when no git repo
// is present, ErrUnsupportedGitState for states it refuses to read partially,
// and ErrDetachedOrUnborn when HEAD has no commit. It NEVER writes to `.git`.
// Refs: ADR-008 §5,§6, MGIT-35
func ReadLocalState(projectRoot string) (*LocalState, error) {
	gitDir, err := resolveGitDir(projectRoot)
	if err != nil {
		return nil, err
	}
	if err := assertSupportedState(gitDir); err != nil {
		return nil, err
	}
	return resolveHead(gitDir)
}

// resolveGitDir resolves the real git directory for projectRoot, handling the
// three on-disk shapes of `.git`. A symlinked `.git` is followed by os.Stat; a
// `.git` FILE is parsed for its `gitdir: <path>` pointer (relative paths are
// resolved against projectRoot). Refs: ADR-008 §6
func resolveGitDir(projectRoot string) (string, error) {
	dotGit := filepath.Join(projectRoot, ".git")
	// Stat (not Lstat) so a symlinked `.git` is followed to its target.
	info, err := os.Stat(dotGit)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: no .git at %s", ErrNoGit, projectRoot)
		}
		return "", fmt.Errorf("stat .git: %w", err)
	}
	if info.IsDir() {
		return dotGit, nil
	}
	return resolveGitDirFile(projectRoot, dotGit)
}

// resolveGitDirFile parses a `.git` FILE (used by linked git worktrees and
// submodules) for its `gitdir: <path>` pointer and returns the pointed-to
// directory. The path may be absolute or relative to projectRoot.
// Refs: ADR-008 §6
func resolveGitDirFile(projectRoot, dotGitFile string) (string, error) {
	data, err := os.ReadFile(dotGitFile) //nolint:gosec // read-only, project-root-derived
	if err != nil {
		return "", fmt.Errorf("read .git file: %w", err)
	}
	const prefix = "gitdir:"
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("%w: .git file lacks a gitdir pointer", ErrUnsupportedGitState)
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if target == "" {
		return "", fmt.Errorf("%w: empty gitdir pointer", ErrUnsupportedGitState)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(projectRoot, target)
	}
	info, err := os.Stat(target) //nolint:gosec // read-only stat of the project's own gitdir pointer; never opened for write
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: gitdir pointer %q is not a directory", ErrUnsupportedGitState, target)
	}
	return target, nil
}

// assertSupportedState refuses git states that a naive HEAD read would
// silently mis-capture. A `shallow` marker (shallow clone) or any sparse-checkout
// config means the working tree is not the full tree, so syncing the base from
// it would corrupt the base — fail loud instead. git-LFS is detected later, at
// the file-content layer, and left as a clearly-flagged follow-up.
//
// The `shallow` marker and `core.sparseCheckout` config live in the COMMON git
// dir, which differs from gitDir when `.git` is a linked-worktree pointer; we
// resolve it via `commondir` so these states are detected from inside a linked
// worktree too (not just the main checkout). Refs: ADR-008 §6
func assertSupportedState(gitDir string) error {
	cdir := commonDir(gitDir)
	if _, err := os.Stat(filepath.Join(cdir, "shallow")); err == nil {
		return fmt.Errorf("%w: shallow clone (mgit base needs the full tree)", ErrUnsupportedGitState)
	}
	if sparse, err := sparseCheckoutEnabled(cdir); err != nil {
		return err
	} else if sparse {
		return fmt.Errorf("%w: sparse-checkout (working tree is a partial subset)", ErrUnsupportedGitState)
	}
	return nil
}

// commonDir resolves the COMMON git directory for gitDir. For a normal `.git`
// dir there is no `commondir` file and the common dir IS gitDir; for a linked
// git worktree (gitDir = .git/worktrees/<name>) the `commondir` file points at
// the shared dir (relative to gitDir or absolute). Refs: ADR-008 §6
func commonDir(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir")) //nolint:gosec // read-only, internal
	if err != nil {
		return gitDir
	}
	rel := strings.TrimSpace(string(data))
	if rel == "" {
		return gitDir
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Clean(filepath.Join(gitDir, rel))
}

// sparseCheckoutEnabled reports whether `core.sparseCheckout = true` is set in
// the repo config. A sparse working tree is only a partial subset of the real
// tree; capturing it as the base would silently drop files. Refs: ADR-008 §6
func sparseCheckoutEnabled(gitDir string) (bool, error) {
	f, err := os.Open(filepath.Join(gitDir, "config")) //nolint:gosec // read-only, internal
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read git config: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only close
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.ToLower(strings.TrimSpace(scanner.Text()))
		line = strings.ReplaceAll(line, " ", "")
		if line == "sparsecheckout=true" {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scan git config: %w", err)
	}
	return false, nil
}

// resolveHead resolves the HEAD of the given git dir to a concrete commit id.
// HEAD itself is per-worktree (read from gitDir), but the branch ref it names
// is resolved against the COMMON dir, where shared refs/heads/* live for linked
// worktrees (gitDir == common dir for a normal checkout). Symbolic-ref chains
// are followed (bounded). Returns ErrDetachedOrUnborn when HEAD points at a ref
// that has no commit yet. Refs: ADR-008 §5,§6
func resolveHead(gitDir string) (*LocalState, error) {
	headData, err := os.ReadFile(filepath.Join(gitDir, "HEAD")) //nolint:gosec // read-only, internal
	if err != nil {
		return nil, fmt.Errorf("read HEAD: %w", err)
	}
	head := strings.TrimSpace(string(headData))
	if !strings.HasPrefix(head, "ref:") {
		// Detached HEAD: HEAD is a raw commit id.
		if isFullHexSHA1(head) {
			return &LocalState{HeadCommit: head}, nil
		}
		return nil, fmt.Errorf("%w: detached HEAD is not a commit id", ErrDetachedOrUnborn)
	}
	refName := strings.TrimSpace(strings.TrimPrefix(head, "ref:"))
	commit, err := resolveRef(gitDir, commonDir(gitDir), refName)
	if err != nil {
		return nil, err
	}
	return &LocalState{HeadCommit: commit, Ref: refName}, nil
}

// resolveRef resolves a fully-qualified ref name to its commit id. It follows
// chained symbolic refs ("ref: refs/heads/<x>" inside a ref file — legal git)
// up to maxSymrefDepth, looking for the loose ref in the per-worktree gitDir
// first then the common dir, and finally falling back to the common dir's
// packed-refs. Returns ErrDetachedOrUnborn when the ref has no commit yet (an
// unborn branch). A symref that loops or nests too deep fails loud as an
// unsupported state rather than being silently misread as unborn. Refs: ADR-008 §6
func resolveRef(gitDir, cdir, refName string) (string, error) {
	for depth := 0; depth < maxSymrefDepth; depth++ {
		val, ok := readLooseRef(gitDir, cdir, refName)
		if !ok {
			return resolvePackedRef(cdir, refName)
		}
		if rest, isSym := strings.CutPrefix(val, "ref:"); isSym {
			refName = strings.TrimSpace(rest)
			continue
		}
		if isFullHexSHA1(val) {
			return val, nil
		}
		return "", fmt.Errorf("%w: ref %s is not a commit id", ErrDetachedOrUnborn, refName)
	}
	return "", fmt.Errorf("%w: symbolic ref chain too deep at %s", ErrUnsupportedGitState, refName)
}

// readLooseRef reads a loose ref file, trying the per-worktree gitDir first
// (covers per-worktree refs and the normal checkout where gitDir == cdir) then
// the common dir (where shared refs/heads/* live for linked worktrees). The
// returned bool is false when no loose ref file exists in either location.
func readLooseRef(gitDir, cdir, refName string) (string, bool) {
	for _, base := range []string{gitDir, cdir} {
		data, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(refName))) //nolint:gosec // read-only, internal
		if err == nil {
			return strings.TrimSpace(string(data)), true
		}
	}
	return "", false
}

// resolvePackedRef scans the common dir's packed-refs for the given ref name
// (packed refs are never symbolic). Returns ErrDetachedOrUnborn when neither a
// loose ref nor a packed entry exists. Refs: ADR-008
func resolvePackedRef(cdir, refName string) (string, error) {
	f, err := os.Open(filepath.Join(cdir, "packed-refs")) //nolint:gosec // read-only, internal
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: ref %s has no commit (unborn branch?)", ErrDetachedOrUnborn, refName)
		}
		return "", fmt.Errorf("read packed-refs: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only close
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == refName && isFullHexSHA1(fields[0]) {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan packed-refs: %w", err)
	}
	return "", fmt.Errorf("%w: ref %s has no commit (unborn branch?)", ErrDetachedOrUnborn, refName)
}

// isFullHexSHA1 reports whether s is a full 40-char lowercase-or-mixed hex
// SHA-1 string.
func isFullHexSHA1(s string) bool {
	if len(s) != hexSHA1Len {
		return false
	}
	for _, c := range s {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
