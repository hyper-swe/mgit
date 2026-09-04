package microvm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// ErrGuestViewNotSettled reports a sync whose bytes landed on the host while
// the guest still read something else when the bound ran out.
//
// From MGIT-192: `mgit sandbox sync` returned success and a guest command
// launched right after read the OLD bytes — twice in the field, then measured
// at 0.1s to over 1.2s of staleness on libkrun. The host-side read-back
// (MGIT-164) hashes the host's directory, where the bytes are complete; the
// guest's kernel keeps its own attribute and page cache for a window after
// its last access, and a host write inside that window is invisible to it.
// Refs: MGIT-192, MGIT-164
var ErrGuestViewNotSettled = errors.New("the guest's view of the worktree did not settle")

const (
	settleBudgetDefault = 5 * time.Second        // measured staleness peaked at ~1.2s; 4× headroom
	settlePollDefault   = 100 * time.Millisecond // one probe costs one exec round trip
	settleExecTimeout   = 20 * time.Second       // a probe that hangs must not hold the sync lock forever
	settleArgvChunk     = 2000                   // sha256sum argv per exec, under the guest's argv cap
)

// settleRequest names what the guest must confirm it reads: the staged digest
// of every delivered path, and the absence of every deleted one. Paths are
// worktree-relative; the guest mounts the worktree at the host's own path.
type settleRequest struct {
	sandboxID string
	worktree  string
	want      worktreesync.Manifest
	deleted   []string
}

// settleView is one probe's answer: which paths the guest still reads
// differently, or why it could not be asked at all. A non-empty unverifiable
// is a loud "cannot tell", never a pass. Refs: R-H300
type settleView struct {
	stale        []string
	unverifiable string
}

// guestSettler asks a running guest what it reads for the delivered paths.
type guestSettler interface {
	Probe(ctx context.Context, req settleRequest) (settleView, error)
}

// defaultSettler picks the settler the manager can actually run: the exec
// channel when there is one, and otherwise an honest "could not ask".
func defaultSettler(m *Manager) guestSettler {
	if m.cfg.GuestDialer == nil {
		return noExecSettler{}
	}
	return execSettler{m: m}
}

// settleGuest waits, within the budget, for the guest to read what was just
// delivered. It returns a note for the report when the guest could not be
// asked, and ErrGuestViewNotSettled when it was asked and never agreed.
// A dry run, a no-op, or an empty delivery asks nothing. Refs: MGIT-192
func (m *Manager) settleGuest(ctx context.Context, sb *sandbox, res worktreesync.Result) (string, error) {
	if res.DryRun || res.Skipped || len(res.Updated)+len(res.Deleted) == 0 {
		return "", nil
	}
	req := settleRequest{sandboxID: sb.info.ID, worktree: sb.info.WorktreePath,
		want: res.Entries, deleted: res.Deleted}
	// The bound is counted in probes, not read from a clock, so a frozen test
	// clock cannot turn a finite wait into an infinite one.
	probes := int(m.settleBudget/m.settlePoll) + 1
	for attempt := 1; ; attempt++ {
		view, err := m.settler.Probe(ctx, req)
		if err != nil {
			return "", fmt.Errorf("asking the guest what it reads: %w", err)
		}
		if view.unverifiable != "" {
			m.cfg.Logger.Warn("worktree sync could not verify the guest's view",
				"event", "sync_unverified", "sandbox_id", sb.info.ID, "reason", view.unverifiable)
			return "delivered on the host, but not verified from inside the guest: " + view.unverifiable, nil
		}
		if len(view.stale) == 0 {
			return "", nil
		}
		if attempt >= probes {
			return "", fmt.Errorf("%w within %s: delivered on the host, but the guest still reads %d path(s) "+
				"differently: %s — retry the sync; if it persists, the guest is not reading the tree that was written",
				ErrGuestViewNotSettled, m.settleBudget, len(view.stale), strings.Join(view.stale, ", "))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(m.settlePoll):
		}
	}
}

// noExecSettler is the settler of a manager with no exec channel: it cannot
// ask, and says so. Refs: R-H300
type noExecSettler struct{}

func (noExecSettler) Probe(context.Context, settleRequest) (settleView, error) {
	return settleView{unverifiable: "this backend has no exec channel into the guest"}, nil
}

// execSettler asks the guest through its exec channel, using only what every
// Linux guest already has — a shell, /proc, and sha256sum — because the guest
// binaries are frozen at compose time and a base composed before this fix
// must still be verifiable. Refs: MGIT-192, MGIT-174
type execSettler struct{ m *Manager }

// Probe invalidates the guest's cached view and then hashes the delivered
// paths from inside the guest. The invalidation is best effort and measured
// effective at once on libkrun (MGIT-192, rounds 5 and 6); the verdict is
// the read that follows it, never the invalidation's exit code.
func (s execSettler) Probe(ctx context.Context, req settleRequest) (settleView, error) {
	// The drop's result is deliberately not consulted: a guest without /proc
	// or a shell simply keeps its cache, and the hash below still decides.
	_, _ = s.run(ctx, req.sandboxID, []string{"sh", "-c", "sync; echo 2 > /proc/sys/vm/drop_caches"})
	got := map[string]string{}
	for _, chunk := range chunkPaths(regularPaths(req.want), settleArgvChunk) {
		argv := append([]string{"sha256sum", "--"}, absolute(req.worktree, chunk)...)
		res, err := s.run(ctx, req.sandboxID, argv)
		if note, missing := toolMissing("sha256sum", res, err); missing {
			return settleView{unverifiable: note}, nil
		}
		if err != nil {
			return settleView{}, err
		}
		for path, hash := range parseSha256sum(string(res.Stdout)) {
			got[relative(req.worktree, path)] = hash
		}
	}
	still, note, err := s.stillPresent(ctx, req)
	if err != nil {
		return settleView{}, err
	}
	if note != "" {
		return settleView{unverifiable: note}, nil
	}
	return classifyGuestView(req.want, got, req.deleted, still), nil
}

// stillPresent lists the deleted paths the guest can still see.
func (s execSettler) stillPresent(ctx context.Context, req settleRequest) ([]string, string, error) {
	if len(req.deleted) == 0 {
		return nil, "", nil
	}
	const script = `for p in "$@"; do [ -e "$p" ] || [ -L "$p" ] && printf '%s\n' "$p"; done; exit 0`
	argv := append([]string{"sh", "-c", script, "sh"}, absolute(req.worktree, req.deleted)...)
	res, err := s.run(ctx, req.sandboxID, argv)
	if note, missing := toolMissing("sh", res, err); missing {
		return nil, note, nil
	}
	if err != nil {
		return nil, "", err
	}
	var still []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		if line != "" {
			still = append(still, relative(req.worktree, line))
		}
	}
	return still, "", nil
}

func (s execSettler) run(ctx context.Context, id string, argv []string) (*model.ExecResult, error) {
	return s.m.execUntilTheGuestAnswers(ctx, id, model.ExecRequest{Command: argv, Timeout: settleExecTimeout})
}

// toolMissing recognizes a guest that lacks the tool a probe needs, which is
// "cannot tell" rather than "stale" or "settled".
func toolMissing(tool string, res *model.ExecResult, err error) (string, bool) {
	note := "the guest has no " + tool + ", so what it reads cannot be verified from inside it"
	if err != nil && strings.Contains(err.Error(), "not found") {
		return note, true
	}
	if res != nil && res.ExitCode == 127 {
		return note, true
	}
	return "", false
}

// parseSha256sum reads sha256sum's "<hex>  <path>" lines into a path→digest
// map. Lines that are not digests (its own error lines go to stderr, but a
// guest may still interleave) are skipped.
func parseSha256sum(out string) map[string]string {
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		hash, path, ok := strings.Cut(line, "  ")
		if !ok || len(hash) != 64 && len(hash) < 6 || strings.ContainsAny(hash, " :") {
			continue
		}
		got[strings.TrimPrefix(path, "*")] = hash
	}
	return got
}

// classifyGuestView compares what the guest read with what was staged.
// Symlinks are recorded by target text, which sha256sum cannot produce, so
// they are confirmed by the host read-back alone. Refs: MGIT-192
func classifyGuestView(want worktreesync.Manifest, got map[string]string,
	deleted, stillPresent []string) settleView {
	var view settleView
	for _, path := range regularPaths(want) {
		hash, ok := got[path]
		switch {
		case !ok:
			view.stale = append(view.stale, path+" (guest cannot read it)")
		case hash != want[path].Hash:
			view.stale = append(view.stale, path+" (guest reads bytes that were not delivered)")
		}
	}
	wasDeleted := map[string]bool{}
	for _, path := range deleted {
		wasDeleted[path] = true
	}
	for _, path := range stillPresent {
		if wasDeleted[path] {
			view.stale = append(view.stale, path+" (still present in the guest)")
		}
	}
	return view
}

// regularPaths lists the manifest's regular files in a stable order.
func regularPaths(want worktreesync.Manifest) []string {
	paths := make([]string, 0, len(want))
	for path, entry := range want {
		if entry.Mode&fs.ModeSymlink == 0 {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// chunkPaths splits paths into argv-sized batches.
func chunkPaths(paths []string, size int) [][]string {
	var chunks [][]string
	for len(paths) > size {
		chunks = append(chunks, paths[:size])
		paths = paths[size:]
	}
	if len(paths) > 0 {
		chunks = append(chunks, paths)
	}
	return chunks
}

func absolute(root string, rels []string) []string {
	out := make([]string, 0, len(rels))
	for _, rel := range rels {
		out = append(out, filepath.Join(root, rel))
	}
	return out
}

func relative(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return rel
	}
	return path
}

// VerifyGuestView asks the sandbox's guest, once and without waiting, what it
// reads for every path in the last delivered manifest. It is the doctor's
// question, not the sync's: a disagreement is reported, never waited out or
// repaired here. Refs: MGIT-164, MGIT-192
func (m *Manager) VerifyGuestView(ctx context.Context, id string) (*model.GuestViewReport, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", model.ErrSandboxNotFound, id)
	}
	if sb.info.State != model.StateRunning {
		return &model.GuestViewReport{Unverifiable: fmt.Sprintf("sandbox %q is %s, not running", id, sb.info.State)}, nil
	}
	if stagedTreePath(sb.dir) == "" {
		return &model.GuestViewReport{Unverifiable: "this backend delivers the worktree as a launch-time image; " +
			"nothing is delivered after launch to compare the guest against"}, nil
	}
	sb.syncMu.Lock()
	defer sb.syncMu.Unlock()
	delivered, err := worktreesync.LoadManifest(sb.dir)
	if err != nil {
		return nil, fmt.Errorf("read the delivered manifest: %w", err)
	}
	if len(delivered) == 0 {
		return &model.GuestViewReport{Unverifiable: "nothing has been delivered to this sandbox yet"}, nil
	}
	view, err := m.settler.Probe(ctx, settleRequest{sandboxID: sb.info.ID, worktree: sb.info.WorktreePath, want: delivered})
	if err != nil {
		return nil, fmt.Errorf("asking the guest what it reads: %w", err)
	}
	return &model.GuestViewReport{Checked: len(delivered), Stale: view.stale, Unverifiable: view.unverifiable}, nil
}
