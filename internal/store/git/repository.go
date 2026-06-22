// Package git wraps go-git v5 plumbing API for mgit.
// This layer provides deterministic, auditable git operations.
// go-git types are never exposed to callers — all data flows
// through model types defined in internal/model/.
// Refs: FR-1, NFR-4, ADR-001
package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/go-git/go-billy/v5/osfs"

	"github.com/hyper-swe/mgit/internal/model"
)

const mgitDirName = ".mgit"

// Repository wraps a go-git repository stored under .mgit/.
// It provides clock injection for deterministic testing and
// encapsulates all go-git interactions.
// Refs: FR-1, MGIT-2.2.1
type Repository struct {
	root  string            // Project root directory
	repo  *gogit.Repository // Underlying go-git repository (never exposed)
	clock func() time.Time  // Injected clock for deterministic timestamps
}

// Init initializes a new mgit repository at the given path.
// Creates the .mgit/ directory with go-git storage (HEAD, objects/, refs/)
// and an initial empty commit on the main branch.
// Returns an error if .mgit/ already exists.
// Refs: FR-1.1, FR-1.2
func Init(path string, clock func() time.Time) (*Repository, error) {
	if clock == nil {
		return nil, errors.New("clock must not be nil")
	}

	mgitPath := filepath.Join(path, mgitDirName)

	// Check if .mgit already exists
	info, err := os.Stat(mgitPath)
	if err == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("repository already exists at %s", mgitPath)
		}
		return nil, fmt.Errorf(".mgit exists but is not a directory at %s", mgitPath)
	}

	// Create .mgit/ directory
	if err := os.MkdirAll(mgitPath, 0o750); err != nil {
		return nil, fmt.Errorf("create .mgit dir: %w", err)
	}

	// Initialize go-git storage at .mgit/ (HEAD, objects/, refs/ inside) as a
	// BARE/worktree-less store (worktree=nil). go-git writes a `.git` gitfile
	// at any worktree root, so passing the project root as the worktree would
	// (a) hijack the `.git` slot in an empty dir and (b) fail outright in a
	// real project whose `.git` is already a directory. mgit drives the store
	// purely via plumbing and reads/writes project files itself via Root().
	// Refs: MGIT-14, ADR-001 (amendment 2026-06-22)
	dotFS := osfs.New(mgitPath)
	storage := filesystem.NewStorage(dotFS, cache.NewObjectLRUDefault())

	repo, err := gogit.Init(storage, nil)
	if err != nil {
		return nil, fmt.Errorf("init go-git repo: %w", err)
	}

	r := &Repository{
		root:  path,
		repo:  repo,
		clock: clock,
	}

	// Create initial empty commit so HEAD is valid
	if err := r.createInitialCommit(); err != nil {
		return nil, fmt.Errorf("create initial commit: %w", err)
	}

	return r, nil
}

// Open opens an existing mgit repository at the given path.
// Validates that .mgit/ exists and contains a valid go-git repository.
// Refs: FR-1.3
func Open(path string, clock func() time.Time) (*Repository, error) {
	if clock == nil {
		return nil, errors.New("clock must not be nil")
	}

	mgitPath := filepath.Join(path, mgitDirName)

	info, err := os.Stat(mgitPath)
	if err != nil {
		return nil, fmt.Errorf("%w: .mgit not found at %s", model.ErrStorageError, path)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: .mgit is not a directory", model.ErrStorageError)
	}

	// Open the self-contained .mgit store as bare (worktree=nil) — the same
	// worktree-less model used by Init. Never opens or reads the project .git.
	// Refs: MGIT-14, ADR-001 (amendment 2026-06-22)
	dotFS := osfs.New(mgitPath)
	storage := filesystem.NewStorage(dotFS, cache.NewObjectLRUDefault())

	repo, err := gogit.Open(storage, nil)
	if err != nil {
		return nil, fmt.Errorf("open go-git repo: %w", err)
	}

	return &Repository{
		root:  path,
		repo:  repo,
		clock: clock,
	}, nil
}

// Close performs cleanup of the repository.
func (r *Repository) Close() error {
	r.repo = nil
	return nil
}

// Root returns the project root directory (parent of .mgit/).
func (r *Repository) Root() string {
	return r.root
}

// MgitDir returns the path to the .mgit/ directory.
func (r *Repository) MgitDir() string {
	return filepath.Join(r.root, mgitDirName)
}

// Now returns the current time from the injected clock.
func (r *Repository) Now() time.Time {
	return r.clock()
}

// WriteRawObject stores a raw git object — its canonical content (the
// bytes after the type/size header) — of the given type in the .mgit
// object store, returning its content-addressed hash. It is idempotent:
// the store is content-addressed, so writing the same bytes twice yields
// the same hash and is a harmless no-op. The sandbox land path uses it to
// import verified guest objects (commits, trees, blobs) into the host
// store host-side, after VerifyBinding (FR-17.5, FR-17.24).
func (r *Repository) WriteRawObject(typ plumbing.ObjectType, content []byte) (string, error) {
	obj := &plumbing.MemoryObject{}
	obj.SetType(typ)
	if _, err := obj.Write(content); err != nil {
		return "", fmt.Errorf("git: stage %s object: %w", typ, err)
	}
	h, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return "", fmt.Errorf("git: store %s object: %w", typ, err)
	}
	return h.String(), nil
}

// Head returns the SHA-1 hash of the current HEAD commit.
// Refs: FR-1.4
func (r *Repository) Head() (string, error) {
	ref, err := r.repo.Head()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	return ref.Hash().String(), nil
}

// CurrentBranch returns the short name of the branch HEAD currently
// points at. Returns an error if HEAD is detached.
// Refs: FR-1.4, FR-8.4
func (r *Repository) CurrentBranch() (string, error) {
	ref, err := r.repo.Head()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	if !ref.Name().IsBranch() {
		return "", fmt.Errorf("HEAD is detached")
	}
	return ref.Name().Short(), nil
}

// createInitialCommit creates an empty initial commit so HEAD is valid, built
// entirely via plumbing (empty tree object + parentless commit object) — never
// via a go-git worktree. Sets the main branch ref and points HEAD at it.
// Refs: FR-1.2, MGIT-14, ADR-001 (amendment 2026-06-22)
func (r *Repository) createInitialCommit() error {
	st := r.repo.Storer

	treeHash, err := emptyTree(st)
	if err != nil {
		return fmt.Errorf("init empty tree: %w", err)
	}

	commitHash, err := writeCommit(st, commitParams{
		tree:    treeHash,
		message: "mgit: initial commit",
		authorAt: object.Signature{
			Name:  "mgit",
			Email: "mgit@system",
			When:  r.clock(),
		},
	})
	if err != nil {
		return fmt.Errorf("init commit: %w", err)
	}

	// Ensure main branch ref exists pointing to the initial commit.
	mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commitHash)
	if err := st.SetReference(mainRef); err != nil {
		return fmt.Errorf("set main ref: %w", err)
	}

	// Point HEAD to the main branch.
	headRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))
	if err := st.SetReference(headRef); err != nil {
		return fmt.Errorf("set HEAD to main: %w", err)
	}

	return nil
}
