package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/hyper-swe/mgit/internal/model"
)

// fullHashLen is the length of a full hex SHA-1 git object ID.
const fullHashLen = 40

// minAbbrevLen is the shortest commit-hash prefix mgit will resolve. Below
// this an abbreviation is too collision-prone to be a meaningful reference.
const minAbbrevLen = 4

// hexPrefixPattern matches a (possibly abbreviated) lowercased hex object-ID
// prefix; resolution is hex-only so non-hash arguments fail fast.
var hexPrefixPattern = regexp.MustCompile(`^[0-9a-f]+$`)

// taskPrefixPattern extracts the task ID from a commit message's structured
// `[MGIT:TASK_ID]` prefix — the authoritative on-object task tag written by
// the commit service (FR-2). Reading it here lets the read path surface
// provenance from the self-contained git object. Refs: FR-2, MGIT-19
var taskPrefixPattern = regexp.MustCompile(`^\[MGIT:([^\]]+)\]`)

// CommitStore manages commit objects in the go-git store.
// It creates go-git commit objects and computes SHA-256 content hashes
// per the dual-hash model (ADR-002).
// Refs: FR-2, FR-3, ADR-002, MGIT-2.2.2
type CommitStore struct {
	repo *Repository
}

// NewCommitStore creates a CommitStore backed by the given Repository.
func NewCommitStore(repo *Repository) *CommitStore {
	return &CommitStore{repo: repo}
}

// CreateCommit creates a new commit in the .mgit object store entirely via
// plumbing — it builds the tree from the current HEAD tree plus mgit's own
// staging set (reading staged file content from disk via Repository.Root()),
// writes the blob/tree/commit objects directly, and advances the current
// branch ref. It NEVER uses a go-git worktree or the project's `.git`/index.
// It populates the commit's CommitID (SHA-1), ContentHash (SHA-256), and the
// timestamp from the injected clock. Staging is cleared on success.
// Refs: FR-2, FR-3, ADR-002, MGIT-14.3
func (cs *CommitStore) CreateCommit(_ context.Context, c *model.Commit) (string, error) {
	goRepo := cs.repo.repo

	// Set timestamp from injected clock.
	c.CreatedAt = cs.repo.Now()

	// Resolve the current branch ref and parent commit.
	headRef, err := goRepo.Head()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	parentHash := headRef.Hash()
	c.ParentID = parentHash.String()

	// Build the new tree from HEAD + staged working files via plumbing.
	treeHash, err := cs.buildTreeFromStaging()
	if err != nil {
		return "", err
	}
	c.TreeHash = treeHash.String()

	commitHash, err := writeCommit(goRepo.Storer, commitParams{
		tree:    treeHash,
		parents: []plumbing.Hash{parentHash},
		message: c.Message,
		authorAt: object.Signature{
			Name:  c.AgentID,
			Email: c.AgentID + "@mgit",
			When:  c.CreatedAt,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create commit: %w", err)
	}

	sha1Hex := commitHash.String()
	c.CommitID = sha1Hex
	// Compute SHA-256 content hash (mgit integrity per ADR-002).
	c.ContentHash = c.ComputeContentHash()

	// Advance the current branch ref to the new commit with a compare-and-set
	// against the parent we read: a concurrent commit that already moved the ref
	// makes this fail loudly rather than orphaning the earlier commit (#5).
	if err := advanceBranchRefCAS(goRepo.Storer, headRef.Name(), commitHash, parentHash); err != nil {
		return "", fmt.Errorf("update ref: %w", err)
	}

	// Staged changes are now committed; reset the staging area.
	if err := cs.repo.clearStaging(); err != nil {
		return "", fmt.Errorf("clear staging: %w", err)
	}

	return sha1Hex, nil
}

// buildTreeFromStaging constructs the next commit's tree by starting from the
// HEAD tree and applying mgit's staged paths: a staged path present on disk is
// written as a blob and added/updated; a staged path absent on disk is treated
// as a deletion. Unstaged working-tree changes are intentionally NOT committed,
// matching git's index semantics. Returns the new tree's hash.
// Refs: MGIT-14.3
func (cs *CommitStore) buildTreeFromStaging() (plumbing.Hash, error) {
	// Start from the complete flattened HEAD tree, then layer staged changes on
	// top: a staged path present on disk overwrites/adds its blob; a staged path
	// absent on disk is removed (a deletion).
	files, err := cs.repo.headFiles()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	staged, err := cs.repo.stagedPaths()
	if err != nil {
		return plumbing.ZeroHash, err
	}

	for _, rel := range staged {
		content, mode, err := cs.repo.workingFileContent(rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				delete(files, rel)
				continue
			}
			return plumbing.ZeroHash, err
		}
		blobHash, err := writeBlob(cs.repo.repo.Storer, content)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		files[rel] = blobEntry{hash: blobHash, mode: mode}
	}

	return writeNestedTree(cs.repo.repo.Storer, files)
}

// GetCommit retrieves a commit by its SHA-1 hash from the go-git store.
// The hash may be a full 40-char SHA-1 or an unambiguous abbreviated prefix
// (matching what `mgit log` prints). Returns ErrCommitNotFound if no commit
// matches and ErrAmbiguousHash if a short prefix matches more than one.
// Refs: FR-3, FR-8.7, MGIT-18
func (cs *CommitStore) GetCommit(_ context.Context, hash string) (*model.Commit, error) {
	h, err := cs.resolveCommitHash(hash)
	if err != nil {
		return nil, err
	}
	obj, err := cs.repo.repo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", model.ErrCommitNotFound, hash)
	}
	return commitFromGitObject(obj), nil
}

// resolveCommitHash resolves a full or abbreviated hex commit-hash reference to
// a concrete object hash, mirroring git's prefix semantics. A full 40-char hash
// is returned as-is. A shorter hex prefix (>= minAbbrevLen) is matched against
// every commit reachable in the store: exactly one match is returned, zero
// yields ErrCommitNotFound, and more than one yields ErrAmbiguousHash. A
// non-hex or too-short reference is rejected as not found.
// Refs: FR-3, FR-8.7, MGIT-18
func (cs *CommitStore) resolveCommitHash(ref string) (plumbing.Hash, error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if len(ref) < minAbbrevLen || len(ref) > fullHashLen || !hexPrefixPattern.MatchString(ref) {
		return plumbing.ZeroHash, fmt.Errorf("%w: %s", model.ErrCommitNotFound, ref)
	}
	// A full-length hash needs no scan — address the object directly.
	if len(ref) == fullHashLen {
		return plumbing.NewHash(ref), nil
	}

	iter, err := cs.repo.repo.Storer.IterEncodedObjects(plumbing.CommitObject)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("iterate commits: %w", err)
	}
	defer iter.Close()

	var candidates []string
	if err := iter.ForEach(func(obj plumbing.EncodedObject) error {
		candidates = append(candidates, obj.Hash().String())
		return nil
	}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("scan commits: %w", err)
	}

	match, err := matchHashPrefix(candidates, ref)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%w: %s", err, ref)
	}
	return plumbing.NewHash(match), nil
}

// matchHashPrefix returns the single candidate hash that begins with ref.
// Zero matches yields ErrCommitNotFound; more than one yields ErrAmbiguousHash.
// It is the pure resolution rule (git's prefix semantics) factored out of the
// store iteration so it can be exercised deterministically. Refs: MGIT-18
func matchHashPrefix(candidates []string, ref string) (string, error) {
	match := ""
	for _, h := range candidates {
		if !strings.HasPrefix(h, ref) {
			continue
		}
		if match != "" {
			return "", model.ErrAmbiguousHash
		}
		match = h
	}
	if match == "" {
		return "", model.ErrCommitNotFound
	}
	return match, nil
}

// ListCommits returns all commits reachable from HEAD.
// Refs: FR-3
func (cs *CommitStore) ListCommits(_ context.Context) ([]*model.Commit, error) {
	goRepo := cs.repo.repo

	iter, err := goRepo.Log(&gogit.LogOptions{
		Order: gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("list commits: %w", err)
	}

	var commits []*model.Commit
	err = iter.ForEach(func(c *object.Commit) error {
		commits = append(commits, commitFromGitObject(c))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate commits: %w", err)
	}

	return commits, nil
}

// DeleteCommit always returns ErrAppendOnlyViolation.
// Commits are immutable per FR-12 append-only requirement.
// Refs: FR-12
func (cs *CommitStore) DeleteCommit(_ context.Context, _ string) error {
	return fmt.Errorf("%w: commits are immutable", model.ErrAppendOnlyViolation)
}

// GetFileFromCommit reads the contents of a single file from the tree
// of the given commit. Returns model.ErrCommitNotFound if the commit
// hash does not exist, or model.ErrFileNotFound if the path is absent
// from the commit tree.
// Refs: FR-6.7, MGIT-4.2.8
func (cs *CommitStore) GetFileFromCommit(_ context.Context, hash, path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("get file: path must not be empty")
	}

	goRepo := cs.repo.repo

	h, err := cs.resolveCommitHash(hash)
	if err != nil {
		return nil, err
	}
	commitObj, err := goRepo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", model.ErrCommitNotFound, hash)
	}

	tree, err := commitObj.Tree()
	if err != nil {
		return nil, fmt.Errorf("get tree for commit %s: %w", hash, err)
	}

	file, err := tree.File(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s in commit %s", model.ErrFileNotFound, path, hash)
	}

	contents, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("read file %s in commit %s: %w", path, hash, err)
	}

	return []byte(contents), nil
}

// commitFromGitObject converts a go-git commit object to a model.Commit.
// This wraps the go-git type so it is never exposed to callers. Provenance is
// recovered from the object itself: the task ID is parsed from the structured
// `[MGIT:TASK_ID]` message prefix (FR-2) so read paths (show/log) and
// task-derived operations (cherry-pick) surface a non-empty task_id without a
// round-trip to the index. CommitType defaults to normal — the only type
// persisted in a plain commit object. ContentHash is the authoritative
// SHA-256 from the index and is bound separately by the service read path
// (it depends on file diffs not present in the object). Refs: FR-2, FR-3, MGIT-19
func commitFromGitObject(obj *object.Commit) *model.Commit {
	parentID := ""
	if obj.NumParents() > 0 {
		parentID = obj.ParentHashes[0].String()
	}

	return &model.Commit{
		CommitID:   obj.Hash.String(),
		Message:    obj.Message,
		CreatedAt:  obj.Author.When,
		ParentID:   parentID,
		TreeHash:   obj.TreeHash.String(),
		AgentID:    obj.Author.Name,
		CreatedBy:  obj.Author.Name,
		TaskID:     taskIDFromMessage(obj.Message),
		CommitType: model.CommitTypeNormal,
	}
}

// taskIDFromMessage parses the task ID out of a commit message's structured
// `[MGIT:TASK_ID]` prefix. Returns the zero TaskID when the prefix is absent
// or malformed (e.g. mgit's own initial system commit), so non-task commits
// read back with an empty — never garbage — task_id. Refs: FR-2, MGIT-19
func taskIDFromMessage(message string) model.TaskID {
	m := taskPrefixPattern.FindStringSubmatch(message)
	if m == nil {
		return model.TaskID{}
	}
	tid, err := model.ParseTaskID(m[1])
	if err != nil {
		return model.TaskID{}
	}
	return tid
}

// CommitFromObjectData decodes a canonical git commit object (the raw
// object content, as received over the sandbox land channel) and maps it
// to a model.Commit via the single commitFromGitObject mapping. The land
// boundary uses this to DERIVE a commit's identity-bearing fields from
// the bytes it verified, rather than trust guest-supplied metadata
// (FR-17.24, SEC-06). Fields not present in a commit object (FileDiffs,
// ContentHash) are left zero for the caller to bind separately.
// Refs: FR-17.5, FR-17.24
func CommitFromObjectData(objectData []byte) (*model.Commit, error) {
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.CommitObject)
	if _, err := obj.Write(objectData); err != nil {
		return nil, fmt.Errorf("git: stage commit object: %w", err)
	}
	var c object.Commit
	if err := c.Decode(obj); err != nil {
		return nil, fmt.Errorf("git: decode commit object: %w", err)
	}
	return commitFromGitObject(&c), nil
}

// content_hash is computed by model.Commit.ComputeContentHash (ADR-002),
// the single authoritative definition shared with sandbox land
// re-verification (FR-17.24).
