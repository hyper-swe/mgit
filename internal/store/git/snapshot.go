package git

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/oklog/ulid/v2"

	"github.com/hyper-swe/mgit/internal/model"
)

// snapshotRefPrefix is the ref namespace passive snapshots live in.
//
// It is deliberately NOT under refs/heads. The separation between what the
// system observed and what the agent authored has to be STRUCTURAL — a
// separate namespace with its own retention — rather than a marker on a shared
// trail. MGIT-110 warned that "an autosaved blob presented alongside
// deliberate task commits would be a worse lie than no autosave", and a
// message prefix on the same branch is exactly that lie. Refs: MGIT-110, R-H234
const snapshotRefPrefix = "refs/mgit-snapshots/"

// snapshotAuthorName and snapshotAuthorEmail identify the capturer in the raw
// object, so even someone reading objects directly — with no ref context —
// can tell a passive capture from an authored commit. Refs: MGIT-110
const (
	snapshotAuthorName  = "mgit snapshotter"
	snapshotAuthorEmail = "snapshot@mgit.local"
)

// SnapshotStore captures and reads passive worktree snapshots. Refs: MGIT-110
type SnapshotStore struct {
	repo *Repository
}

// NewSnapshotStore creates a SnapshotStore backed by the given Repository.
func NewSnapshotStore(repo *Repository) *SnapshotStore {
	return &SnapshotStore{repo: repo}
}

// Capture records the CURRENT working tree as an orphan commit under this
// task's snapshot namespace, and returns the resulting Snapshot.
//
// It is passive in the strict sense: it does not stage, does not touch the
// staging area, does not move any branch, and does not require the agent to
// have done anything. Identical content produces an identical tree, so an idle
// worktree costs one tree object and no blobs. Refs: MGIT-110, R-H234
func (ss *SnapshotStore) Capture(ctx context.Context, taskID string, at time.Time) (*model.Snapshot, error) {
	if taskID == "" {
		return nil, errors.New("snapshot: task ID must not be empty")
	}
	fingerprint, err := ss.repo.WorkingTreeFingerprint()
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	treeHash, fileCount, err := ss.writeWorkingTree()
	if err != nil {
		return nil, err
	}
	id, err := newSnapshotID(at)
	if err != nil {
		return nil, err
	}
	commitHash, err := ss.writeSnapshotCommit(taskID, id, fingerprint, treeHash, at)
	if err != nil {
		return nil, err
	}
	ref := plumbing.ReferenceName(snapshotRefPrefix + taskID + "/" + id)
	if err := ss.repo.repo.Storer.SetReference(plumbing.NewHashReference(ref, commitHash)); err != nil {
		return nil, fmt.Errorf("snapshot: set ref: %w", err)
	}
	_ = ctx
	return &model.Snapshot{
		ID: id, TaskID: taskID, CommitHash: commitHash.String(), TreeHash: treeHash.String(),
		Fingerprint: fingerprint, CapturedAt: at.UTC(), FileCount: fileCount,
		Trigger: model.SnapshotTriggerQuiescence,
	}, nil
}

// writeWorkingTree writes every trackable working file as a blob and assembles
// the tree, returning its hash and the file count. It reuses the same file
// enumeration as the working-tree fingerprint, so what is snapshotted is
// exactly what the fingerprint measured — .gitignore honored, .mgit/.git
// excluded. Refs: MGIT-110
func (ss *SnapshotStore) writeWorkingTree() (plumbing.Hash, int, error) {
	paths, err := ss.repo.listWorkingFiles()
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("snapshot: list working files: %w", err)
	}
	files := make(map[string]blobEntry, len(paths))
	for _, rel := range paths {
		content, mode, err := ss.repo.workingFileContent(rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // raced with a delete: the snapshot records what was there
			}
			return plumbing.ZeroHash, 0, fmt.Errorf("snapshot: read %s: %w", rel, err)
		}
		blobHash, err := writeBlob(ss.repo.repo.Storer, content)
		if err != nil {
			return plumbing.ZeroHash, 0, fmt.Errorf("snapshot: write blob %s: %w", rel, err)
		}
		files[filepath.ToSlash(rel)] = blobEntry{hash: blobHash, mode: mode}
	}
	treeHash, err := writeNestedTree(ss.repo.repo.Storer, files)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("snapshot: write tree: %w", err)
	}
	return treeHash, len(files), nil
}

// writeSnapshotCommit writes the ORPHAN commit object wrapping a snapshot
// tree. No parent is the load-bearing detail: with no ancestry, nothing that
// walks a task branch can reach it, so squash and land exclude snapshots by
// construction rather than by remembering to. Refs: MGIT-110, R-H234
func (ss *SnapshotStore) writeSnapshotCommit(taskID, id, fingerprint string, tree plumbing.Hash, at time.Time) (plumbing.Hash, error) {
	sig := object.Signature{Name: snapshotAuthorName, Email: snapshotAuthorEmail, When: at.UTC()}
	c := &object.Commit{
		Author: sig, Committer: sig, TreeHash: tree,
		Message: fmt.Sprintf(
			"mgit snapshot: passive capture of the worktree (NOT an authored commit)\n\n"+
				"task: %s\nsnapshot: %s\ntrigger: %s\nfingerprint: %s\n",
			taskID, id, model.SnapshotTriggerQuiescence, fingerprint),
	}
	obj := ss.repo.repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("snapshot: encode commit: %w", err)
	}
	hash, err := ss.repo.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("snapshot: store commit: %w", err)
	}
	return hash, nil
}

// List returns a task's snapshots, newest first. Refs: MGIT-110
func (ss *SnapshotStore) List(_ context.Context, taskID string) ([]model.Snapshot, error) {
	out, err := ss.collect(snapshotRefPrefix + taskID + "/")
	if err != nil {
		return nil, err
	}
	// ULIDs are lexicographically ordered by time, so a reverse sort on ID is
	// newest-first and does not depend on a clock the caller supplied.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// collect reads every snapshot under a ref prefix. Refs: MGIT-110
func (ss *SnapshotStore) collect(prefix string) ([]model.Snapshot, error) {
	refs, err := ss.repo.repo.References()
	if err != nil {
		return nil, fmt.Errorf("snapshot: list refs: %w", err)
	}
	defer refs.Close()
	var out []model.Snapshot
	err = refs.ForEach(func(r *plumbing.Reference) error {
		name := r.Name().String()
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		snap, err := ss.readSnapshot(r)
		if err != nil {
			return err
		}
		out = append(out, *snap)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readSnapshot reconstructs a Snapshot from its ref and commit object.
func (ss *SnapshotStore) readSnapshot(r *plumbing.Reference) (*model.Snapshot, error) {
	c, err := ss.repo.repo.CommitObject(r.Hash())
	if err != nil {
		return nil, fmt.Errorf("snapshot: read commit %s: %w", r.Hash(), err)
	}
	rest := strings.TrimPrefix(r.Name().String(), snapshotRefPrefix)
	taskID, id, _ := strings.Cut(rest, "/")
	files := 0
	if tree, terr := c.Tree(); terr == nil {
		_ = tree.Files().ForEach(func(*object.File) error { files++; return nil })
	}
	return &model.Snapshot{
		ID: id, TaskID: taskID, CommitHash: c.Hash.String(), TreeHash: c.TreeHash.String(),
		Fingerprint: fieldFromMessage(c.Message, "fingerprint"), CapturedAt: c.Author.When.UTC(),
		FileCount: files, Trigger: fieldFromMessage(c.Message, "trigger"),
	}, nil
}

// fieldFromMessage reads a "key: value" line out of a snapshot commit message.
func fieldFromMessage(msg, key string) string {
	for _, line := range strings.Split(msg, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Prune keeps the newest `keep` snapshots for a task and deletes the rest,
// returning how many were dropped.
//
// Retention is the snapshot namespace's own, separate from anything that
// governs authored commits — snapshots are evidence with a working life, not
// content with a permanent one. Note that deleting the ref makes a snapshot
// unreachable; its objects are reclaimed by `mgit gc`, not by this call.
// Refs: MGIT-110, R-H234
func (ss *SnapshotStore) Prune(ctx context.Context, taskID string, keep int) (int, error) {
	if keep < 0 {
		return 0, fmt.Errorf("snapshot prune: keep must not be negative, got %d", keep)
	}
	snaps, err := ss.List(ctx, taskID)
	if err != nil {
		return 0, err
	}
	if len(snaps) <= keep {
		return 0, nil
	}
	dropped := 0
	for _, s := range snaps[keep:] {
		ref := plumbing.ReferenceName(snapshotRefPrefix + s.TaskID + "/" + s.ID)
		if err := ss.repo.repo.Storer.RemoveReference(ref); err != nil {
			return dropped, fmt.Errorf("snapshot prune: remove %s: %w", ref, err)
		}
		dropped++
	}
	return dropped, nil
}

// Materialize writes a snapshot's tree into dest, which must be empty or not
// exist, and returns the number of files written.
//
// It REFUSES a non-empty destination on purpose. The work a snapshot exists to
// protect is usually still on disk in the worktree it came from, and restoring
// over it would destroy exactly what was being recovered. Recovery writes
// somewhere new; comparing and merging is then the operator's decision, made
// with both copies intact. Refs: MGIT-110, MGIT-109
func (ss *SnapshotStore) Materialize(ctx context.Context, snapshotID, dest string) (int, error) {
	snap, err := ss.find(ctx, snapshotID)
	if err != nil {
		return 0, err
	}
	if err := requireEmptyDir(dest); err != nil {
		return 0, err
	}
	c, err := ss.repo.repo.CommitObject(plumbing.NewHash(snap.CommitHash))
	if err != nil {
		return 0, fmt.Errorf("snapshot restore: read commit: %w", err)
	}
	tree, err := c.Tree()
	if err != nil {
		return 0, fmt.Errorf("snapshot restore: read tree: %w", err)
	}
	written := 0
	err = tree.Files().ForEach(func(f *object.File) error {
		if werr := writeSnapshotFile(dest, f); werr != nil {
			return werr
		}
		written++
		return nil
	})
	if err != nil {
		return written, err
	}
	return written, nil
}

// writeSnapshotFile materializes one tree entry under dest, rejecting any path
// that would escape it. A snapshot's paths come from a worktree scan rather
// than from user input, but a restore writes to a directory the operator named
// and a traversal here would write outside it. Refs: MGIT-110
func writeSnapshotFile(dest string, f *object.File) error {
	full := filepath.Join(dest, filepath.FromSlash(f.Name))
	if rel, err := filepath.Rel(dest, full); err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("snapshot restore: path %q escapes the destination", f.Name)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return fmt.Errorf("snapshot restore: create dir: %w", err)
	}
	content, err := f.Contents()
	if err != nil {
		return fmt.Errorf("snapshot restore: read %s: %w", f.Name, err)
	}
	mode := os.FileMode(0o600)
	if f.Mode.IsFile() {
		if osMode, merr := f.Mode.ToOSFileMode(); merr == nil && osMode&0o111 != 0 {
			mode = 0o700
		}
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		return fmt.Errorf("snapshot restore: write %s: %w", f.Name, err)
	}
	return nil
}

// find locates a snapshot by ID across every task's namespace.
func (ss *SnapshotStore) find(_ context.Context, snapshotID string) (*model.Snapshot, error) {
	all, err := ss.collect(snapshotRefPrefix)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == snapshotID {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("snapshot %q: %w", snapshotID, model.ErrSnapshotNotFound)
}

// requireEmptyDir returns nil when dir does not exist or is empty.
func requireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("snapshot restore: read destination: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("snapshot restore: destination %s is not empty — "+
			"restore to a NEW directory so the work already there is not overwritten", dir)
	}
	return nil
}

// newSnapshotID mints a time-ordered ULID so IDs sort by capture time.
func newSnapshotID(at time.Time) (string, error) {
	id, err := ulid.New(ulid.Timestamp(at.UTC()), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("snapshot: generate id: %w", err)
	}
	return id.String(), nil
}

// Fingerprint reports the current working-tree content fingerprint, so a
// caller can decide whether anything changed without paying for a capture.
// Refs: MGIT-110
func (ss *SnapshotStore) Fingerprint() (string, error) {
	return ss.repo.WorkingTreeFingerprint()
}
