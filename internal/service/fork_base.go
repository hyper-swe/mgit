package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/gitref"
)

// committedFilesReader reads git's committed tree with contents and the HEAD
// commit id; production passes gitref.CommittedFiles. Refs: MGIT-283
type committedFilesReader func(projectRoot string) ([]gitref.CommittedFile, string, error)

// CommittedForkBase builds a NEW task's fork-base from git's committed tree
// and reports the checkout's uncommitted paths, which it leaves out.
//
// A task's base used to be the checkout's LOCAL working state (ADR-008 §2), so
// creating a worktree absorbed uncommitted, possibly private, files into the
// base, a consumer that landed the task's tree landed them, and `mgit status`
// in the main checkout then read clean (MGIT-283). The base is now a commit
// whose tree is exactly what git has committed at the local HEAD — unpushed
// commits included — parented on the current base so history stays connected,
// and created without moving any branch: the main checkout's branch is left
// where it was, so its uncommitted work stays visible as uncommitted.
//
// ok is false when the project has no readable git commit (no git, or an
// unborn HEAD): there is no committed tree to build from, and the caller keeps
// the mgit base as it is. Refs: MGIT-283, ADR-008 §2
func (s *SyncService) CommittedForkBase(_ context.Context) (base string, uncommitted []string, ok bool, err error) {
	files, head, err := s.readCommittedFiles(s.repo.Root())
	if errors.Is(err, gitref.ErrNoGit) || errors.Is(err, gitref.ErrDetachedOrUnborn) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("read git's committed tree: %w", err)
	}
	committed := make(map[string]string, len(files))
	snapshot := make([]gitstore.SnapshotFile, 0, len(files))
	for _, f := range files {
		committed[f.Path] = plumbing.ComputeHash(plumbing.BlobObject, f.Content).String()
		snapshot = append(snapshot, gitstore.SnapshotFile{Path: f.Path, Mode: f.Mode, Content: f.Content})
	}
	if uncommitted, err = s.repo.UncommittedAgainst(committed); err != nil {
		return "", nil, false, fmt.Errorf("compare the checkout with git's committed tree: %w", err)
	}
	parent, err := s.repo.Head()
	if err != nil {
		return "", nil, false, fmt.Errorf("resolve the base: %w", err)
	}
	c := &model.Commit{
		AgentID: "mgit-sync",
		Message: fmt.Sprintf("[mgit-sync] task fork base: git %s's committed tree", short(head)),
	}
	if base, err = s.commitStore.CreateDetachedCommit(c, parent, snapshot); err != nil {
		return "", nil, false, err
	}
	return base, uncommitted, true, nil
}

// UncommittedNow reports the checkout's paths git has not committed, without
// changing anything: what --include-uncommitted is about to capture. Empty
// when the project has no readable git commit. Refs: MGIT-283
func (s *SyncService) UncommittedNow(_ context.Context) ([]string, error) {
	committed, err := s.readCommitted(s.repo.Root())
	if errors.Is(err, gitref.ErrNoGit) || errors.Is(err, gitref.ErrDetachedOrUnborn) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read git's committed tree: %w", err)
	}
	return s.repo.UncommittedAgainst(committed)
}

// withCommittedFilesReader overrides the committed-tree reader (test seam).
// Refs: MGIT-283
func (s *SyncService) withCommittedFilesReader(r committedFilesReader) *SyncService {
	s.readCommittedFiles = r
	return s
}
