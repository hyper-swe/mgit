package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// MGIT-131: `go build -o build/mgit ./cmd/mgit/` (the project's own
// verification checklist) plus `mgit commit -a` (the instructed way to commit)
// swept a 40 MB binary into task/MGIT-121.1 and a 21 MB one into
// task/MGIT-123. These tests pin the tripwire that now refuses that stage, on
// BOTH staging paths — an explicit `add <path>` and the bulk `add .` behind
// `commit -a`. Refs: FR-2, MGIT-131

// writeSizedFile writes a file of exactly n bytes under the repo root.
func writeSizedFile(t *testing.T, root, rel string, n int64) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o750))
	require.NoError(t, os.WriteFile(abs, make([]byte, n), 0o600))
	return abs
}

func TestWorktreeStore_Add_FileOverLimit_RefusedNamingPathAndSize(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo).WithMaxStagedFileBytes(1 << 20)
	writeSizedFile(t, repo.Root(), "build/mgit", 3<<20)

	err := ws.Add(ctx, "build/mgit")

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrFileTooLarge)
	assert.Contains(t, err.Error(), "build/mgit", "the refusal must name the file")
	assert.Contains(t, err.Error(), "3.0 MB", "the refusal must state the file's size")
	assert.Contains(t, err.Error(), "1.0 MB", "the refusal must state the limit it broke")

	// Refusing means refusing: nothing may be left staged.
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Empty(t, staged)
}

func TestWorktreeStore_AddAll_FileOverLimit_RefusesWholeStage(t *testing.T) {
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo).WithMaxStagedFileBytes(1 << 20)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "src.go"), []byte("package src\n"), 0o600))
	writeSizedFile(t, repo.Root(), "build/mgit", 3<<20)

	err := ws.Add(ctx, ".")

	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrFileTooLarge)
	assert.Contains(t, err.Error(), "build/mgit")

	// A partial stage that reported success would be the MGIT-77 defect: the
	// agent would commit believing it captured everything it wrote.
	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Empty(t, staged, "a refused bulk stage must stage nothing at all")
}

func TestWorktreeStore_Add_RefusalNamesBothEscapeHatches(t *testing.T) {
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo).WithMaxStagedFileBytes(1 << 20)
	writeSizedFile(t, repo.Root(), "big.bin", 2<<20)

	err := ws.Add(context.Background(), "big.bin")

	require.Error(t, err)
	// A guard whose override is undiscoverable gets disabled wholesale.
	assert.Contains(t, err.Error(), "--allow-large")
	assert.Contains(t, err.Error(), "mgit config set limits.max_staged_file_mb")
	assert.Contains(t, err.Error(), ".gitignore", "the refusal must name the real fix first")
}

func TestWorktreeStore_Add_SizeGuard_BoundaryAndOverride(t *testing.T) {
	const limit int64 = 1 << 20
	tests := []struct {
		name    string
		size    int64
		limit   int64
		wantErr bool
	}{
		{name: "well_under_limit", size: 1024, limit: limit, wantErr: false},
		{name: "one_byte_under_limit", size: limit - 1, limit: limit, wantErr: false},
		{name: "exactly_at_limit", size: limit, limit: limit, wantErr: false},
		{name: "one_byte_over_limit", size: limit + 1, limit: limit, wantErr: true},
		{name: "over_limit_but_guard_disabled", size: limit + 1, limit: 0, wantErr: false},
		{name: "over_limit_but_guard_negative", size: limit + 1, limit: -1, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initTestRepo(t)
			ws := NewWorktreeStore(repo).WithMaxStagedFileBytes(tt.limit)
			writeSizedFile(t, repo.Root(), "artifact.bin", tt.size)

			err := ws.Add(context.Background(), "artifact.bin")

			if tt.wantErr {
				require.ErrorIs(t, err, model.ErrFileTooLarge)
				return
			}
			require.NoError(t, err)
			staged, sErr := repo.stagedPaths()
			require.NoError(t, sErr)
			assert.Equal(t, []string{"artifact.bin"}, staged)
		})
	}
}

func TestWorktreeStore_Add_DefaultConstruction_GuardOff(t *testing.T) {
	// The store's constructed default is OFF, and the staging surfaces turn it
	// on. The ADR-008 auto-resync stages the project's ALREADY-committed git
	// content via the same Add(".") — refusing there would break `mgit status`
	// on any repo that legitimately tracks a large file, with no author present
	// to act on the refusal. Refs: MGIT-131, ADR-008
	repo := initTestRepo(t)
	ws := NewWorktreeStore(repo)
	writeSizedFile(t, repo.Root(), "huge.bin", (DefaultMaxStagedFileBytes)+1)

	require.NoError(t, ws.Add(context.Background(), "."))
}

func TestWorktreeStore_AddAll_StagedDeletion_NotWeighed(t *testing.T) {
	// A staged deletion puts no bytes in the store, and the file is not on disk
	// to size — the guard must skip it rather than fail the stage.
	repo := initTestRepo(t)
	ctx := context.Background()
	ws := NewWorktreeStore(repo)
	cs := NewCommitStore(repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Root(), "gone.txt"), []byte("bye\n"), 0o600))
	require.NoError(t, ws.Add(ctx, "gone.txt"))
	_, err := cs.CreateCommit(ctx, makeTestModelCommit(t, "MGIT-131"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(repo.Root(), "gone.txt")))

	guarded := NewWorktreeStore(repo).WithMaxStagedFileBytes(1 << 20)
	require.NoError(t, guarded.Add(ctx, "."))

	staged, err := repo.stagedPaths()
	require.NoError(t, err)
	assert.Contains(t, staged, "gone.txt")
}

// TestAssertNotOversized_ThisRepositoryRealContent_DoesNotFire is the test that
// decides whether the threshold is right. A guard that fires on normal work is
// turned off within a week, so it is not enough for the number to look sane
// against synthetic fixtures: it is run here against THIS repository's actual
// working tree, through the same walk `mgit add .` uses (so .gitignore applies)
// and the same guard the staging paths call. If this fails, either a real file
// outgrew the limit — in which case the limit is wrong — or a build artifact is
// sitting in the tree unignored, which is the defect this task exists to stop.
// Refs: MGIT-131
func TestAssertNotOversized_ThisRepositoryRealContent_DoesNotFire(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "locate this test file to find the repository root")
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	r := &Repository{root: root}
	files, err := r.listWorkingFiles()
	require.NoError(t, err)
	require.Greater(t, len(files), 100, "expected the real repository tree, got %d files", len(files))

	ws := &WorktreeStore{repo: r, maxStagedFileBytes: DefaultMaxStagedFileBytes}
	err = ws.assertNotOversized(files)
	require.NoError(t, err, "the default %s limit fires on this repository's own content", humanBytes(DefaultMaxStagedFileBytes))

	// Record the headroom, so a future reader can see how close the largest
	// real file is to the limit rather than taking the number on faith.
	var largest string
	var largestSize int64
	for _, rel := range files {
		info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > largestSize {
			largest, largestSize = rel, info.Size()
		}
	}
	t.Logf("largest stageable file: %s (%s); limit %s; %d files walked",
		largest, humanBytes(largestSize), humanBytes(DefaultMaxStagedFileBytes), len(files))
	assert.Less(t, largestSize*2, DefaultMaxStagedFileBytes,
		"largest real file %s (%s) has less than 2x headroom under the %s limit — revisit the threshold",
		largest, humanBytes(largestSize), humanBytes(DefaultMaxStagedFileBytes))
}

func TestHumanBytes_Magnitudes_ReadableUnits(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want string
	}{
		{name: "bytes", in: 512, want: "512 B"},
		{name: "kilobytes", in: 2048, want: "2.0 KB"},
		{name: "megabytes", in: 5 << 20, want: "5.0 MB"},
		{name: "real_mgit_binary", in: 21689682, want: "20.7 MB"},
		{name: "gigabytes", in: 3 << 30, want: "3.0 GB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, humanBytes(tt.in))
		})
	}
}

func TestOversizedError_Message_IsSingleSelfContainedRefusal(t *testing.T) {
	err := oversizedError("build/mgit", 21689682, DefaultMaxStagedFileBytes)
	msg := err.Error()

	assert.True(t, strings.HasPrefix(msg, model.ErrFileTooLarge.Error()),
		"the sentinel must lead the message: %s", msg)
	assert.Contains(t, msg, "build/mgit is 20.7 MB (limit 5.0 MB)")
}
