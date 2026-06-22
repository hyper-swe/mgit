package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// mgit owns its OWN staging model rather than relying on go-git's `.git/index`
// (which only exists when go-git has a worktree — which mgit deliberately does
// not, so it never writes a `.git` at the project root). The staging area is a
// single JSON file inside .mgit/ recording the set of paths an agent has staged
// for the next commit. It is mgit-internal state, never user content, and lives
// entirely under .mgit/ so it can never collide with the project's git index.
// Refs: MGIT-14.3, ADR-001 (amendment 2026-06-22)

const stagingFileName = "staging.json"

// stagingState is the on-disk representation of the staging area: the sorted
// set of project-relative paths staged for the next commit. Whether a staged
// path is an add/modify or a delete is resolved at commit time by comparing
// the working tree against the HEAD tree, so the staging file only needs the
// path set (stateless w.r.t. content).
type stagingState struct {
	Paths []string `json:"paths"`
}

// stagingPath returns the absolute path of the staging file inside .mgit/.
func (r *Repository) stagingPath() string {
	return filepath.Join(r.MgitDir(), stagingFileName)
}

// loadStaging reads the staging file, returning an empty state if absent.
func (r *Repository) loadStaging() (*stagingState, error) {
	data, err := os.ReadFile(r.stagingPath()) //nolint:gosec // path is mgit-internal
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &stagingState{}, nil
		}
		return nil, fmt.Errorf("read staging: %w", err)
	}
	var s stagingState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode staging: %w", err)
	}
	return &s, nil
}

// saveStaging atomically writes the staging file under .mgit/.
func (r *Repository) saveStaging(s *stagingState) error {
	sort.Strings(s.Paths)
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode staging: %w", err)
	}
	tmp := r.stagingPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write staging: %w", err)
	}
	if err := os.Rename(tmp, r.stagingPath()); err != nil {
		return fmt.Errorf("commit staging: %w", err)
	}
	return nil
}

// stagePath adds a project-relative path to the staging set (idempotent).
func (r *Repository) stagePath(rel string) error {
	return r.stagePaths([]string{rel})
}

// stagePaths adds project-relative paths to the staging set (idempotent),
// loading and rewriting the staging file once for the whole batch.
func (r *Repository) stagePaths(rels []string) error {
	s, err := r.loadStaging()
	if err != nil {
		return err
	}
	existing := make(map[string]bool, len(s.Paths))
	for _, p := range s.Paths {
		existing[p] = true
	}
	for _, rel := range rels {
		if existing[rel] {
			continue
		}
		existing[rel] = true
		s.Paths = append(s.Paths, rel)
	}
	return r.saveStaging(s)
}

// stagedPaths returns the currently staged project-relative paths.
func (r *Repository) stagedPaths() ([]string, error) {
	s, err := r.loadStaging()
	if err != nil {
		return nil, err
	}
	return s.Paths, nil
}

// clearStaging removes the staging file, resetting the staging area. It is a
// no-op if the file does not exist.
func (r *Repository) clearStaging() error {
	err := os.Remove(r.stagingPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear staging: %w", err)
	}
	return nil
}
