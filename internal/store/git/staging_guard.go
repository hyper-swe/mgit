package git

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyper-swe/mgit/internal/model"
)

// DefaultMaxStagedFileBytes is the per-file size above which staging is
// REFUSED unless the caller overrides it. It is 5 MiB, and the number is
// measured, not guessed — a threshold that fires on normal work gets switched
// off within a week, and one that never fires catches nothing.
//
// Measured on this repository (2026-08-17, 833 tracked files):
//
//   - Largest tracked file:            .mtix/tasks.json, 1.09 MB (and growing
//     ~200 KB/month as the task graph grows).
//   - Second largest:                  go.sum, 122 KB. Over 99% of tracked
//     files are under 130 KB.
//   - Largest blob EVER committed:     3.46 MB (.mtix/data/backups/*.db
//     snapshots, since gitignored). That is the high-water mark of anything
//     this project has ever put in git.
//   - Smallest build artifact the
//     documented commands produce:     9.45 MB (mgit-guest). `go build -o
//     build/mgit ./cmd/mgit/` is 20.7 MB, `go test -c` is 12.4 MB, and the
//     binaries actually swept into task branches were 21 MB and 40 MB.
//
// So the threshold has to sit inside (3.46 MB, 9.45 MB): above everything this
// project has legitimately committed, below the smallest thing its own build
// commands emit. 5 MiB is the round number in that window — 4.8x the largest
// tracked file, 1.5x the largest blob ever committed, and 1.9x below the
// smallest build artifact it must catch.
//
// Refs: FR-2.6b, MGIT-131
const DefaultMaxStagedFileBytes int64 = 5 << 20

// WithMaxStagedFileBytes sets the per-file staging size limit and returns the
// receiver for fluent wiring. A value <= 0 DISABLES the guard.
//
// The limit is off in a freshly constructed WorktreeStore and turned on by the
// callers that stage on an author's behalf (`mgit add`, `mgit commit -a`).
// That is deliberate: the ADR-008 auto-resync also calls Add(".") to absorb the
// project's already-committed git content into the mgit base, and a size
// refusal there would break `mgit status` on any repository that legitimately
// tracks a large file — with no author present to act on the refusal, and
// nothing new entering the store anyway.
// Refs: FR-2.6b, MGIT-131, ADR-008
func (ws *WorktreeStore) WithMaxStagedFileBytes(limit int64) *WorktreeStore {
	ws.maxStagedFileBytes = limit
	return ws
}

// assertNotOversized refuses the whole stage when any path exceeds the
// configured per-file limit, naming the first offender and its size.
//
// It refuses the BATCH rather than silently skipping the offending path: a
// partial stage that reports success is the MGIT-77 defect (a success signal
// for work that was not captured). Paths that are not regular files on disk are
// skipped — a staged deletion puts no bytes in the store, and a symlink stores
// its link text, not the target's content.
// Refs: FR-2.6b, MGIT-131, MGIT-77
func (ws *WorktreeStore) assertNotOversized(rels []string) error {
	if ws.maxStagedFileBytes <= 0 {
		return nil
	}
	for _, rel := range rels {
		abs := filepath.Join(ws.repo.root, filepath.FromSlash(rel))
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > ws.maxStagedFileBytes {
			return oversizedError(rel, info.Size(), ws.maxStagedFileBytes)
		}
	}
	return nil
}

// oversizedError builds the refusal a human or agent acts on: it names the
// path, its size, the limit, and BOTH escape hatches. A guard whose override is
// undiscoverable gets disabled wholesale — or worse, worked around by not
// committing at all. Refs: FR-2, MGIT-131
func oversizedError(rel string, size, limit int64) error {
	return fmt.Errorf("%w: %s is %s (limit %s)\n"+
		"  mgit's store is append-only: a file staged once stays in the branch's objects\n"+
		"  forever, even if a later commit deletes it. A file this size is nearly always a\n"+
		"  build artifact — if it is, add it to .gitignore instead of committing it.\n"+
		"  To stage it anyway:   mgit add --allow-large %s   (or: mgit commit -a --allow-large)\n"+
		"  To raise the limit:   mgit config set limits.max_staged_file_mb <N>   (0 disables it)",
		model.ErrFileTooLarge, rel, humanBytes(size), humanBytes(limit), rel)
}

// humanBytes renders a byte count the way the refusal needs to be read at a
// glance: "20.7 MB", not "21689682". Refs: MGIT-131
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
