//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// sameSecondRound performs read → write in the same wall-clock second, or
// reports that the second boundary was crossed so the caller can retry: a
// round that straddles a second measures nothing about MGIT-93, whose whole
// hazard is a cached mtime with 1 s granularity that the write did not move.
func sameSecondRound(t *testing.T, read func() string, write func()) (before string, sameSecond bool) {
	t.Helper()
	before = read()
	readAt := time.Now()
	write()
	wroteAt := time.Now()
	return before, readAt.Unix() == wroteAt.Unix()
}

// TestE2E_Libkrun_RealVM_Sync_SameSecondSameLengthEditIsObserved establishes
// MGIT-93 by measurement, deterministically: a host write of NEW content of
// the SAME length, landing in the SAME second as the guest's last read, keeps
// the guest's cached size and mtime identical — so only the sync verb's own
// settle (MGIT-192: invalidate, then hash from inside the guest) can make the
// guest observe it.
//
// Two arms. The RAW arm writes straight into the staged tree the guest mounts
// and reads again without any sync: that is the hazard itself, and its miss
// count is the measurement the ticket asked for (logged, not asserted — the
// share's caching is not ours to fix). The VERB arm does the same edit on the
// host worktree and delivers it through the production SyncWorktree: every
// round MUST be observed, because a sync that returns success while the guest
// executes stale code is the silent-staleness class the verb exists to
// prevent. Refs: MGIT-93, MGIT-192, MGIT-90, MGIT-71
func TestE2E_Libkrun_RealVM_Sync_SameSecondSameLengthEditIsObserved(t *testing.T) {
	requireRealVM(t)
	sb := launchRealVMForSync(t, "samesec", "MGIT-93")
	hostPath := filepath.Join(sb.worktree, "seed.txt")
	stagedPath := filepath.Join(sb.staged, "seed.txt")
	if got := guestRead(t, sb, hostPath); got != "host work" {
		t.Fatalf("precondition: the guest must read the delivered seed, got %q", got)
	}
	const rounds, maxTries = 6, 12
	// Every payload is exactly the seed's length ("host work\n" = 10 bytes).
	payload := func(arm string, i int) string { return fmt.Sprintf("%s wrk%d\n", arm, i) }

	// RAW arm: the share alone.
	rawMisses, rawRounds := 0, 0
	for i, tries := 0, 0; i < rounds && tries < maxTries; tries++ {
		want := payload("raw ", i)
		_, same := sameSecondRound(t,
			func() string { return guestRead(t, sb, hostPath) },
			func() {
				if err := os.WriteFile(stagedPath, []byte(want), 0o600); err != nil {
					t.Fatal(err)
				}
			})
		if !same {
			continue // the second ticked between read and write; measure nothing
		}
		time.Sleep(300 * time.Millisecond)
		got := guestRead(t, sb, hostPath) + "\n"
		rawRounds++
		if got != want {
			rawMisses++
		}
		i++
	}
	t.Logf("MEASUREMENT (raw share, no sync): %d of %d same-second same-length edits were NOT observed by the guest 300 ms later",
		rawMisses, rawRounds)
	if rawRounds < rounds/2 {
		t.Fatalf("only %d raw rounds landed in one second; the machine is too slow for this measurement", rawRounds)
	}

	// VERB arm: the production sync must deliver every one.
	verbRounds := 0
	for i, tries := 0, 0; i < rounds && tries < maxTries; tries++ {
		want := payload("verb", i)
		_, same := sameSecondRound(t,
			func() string { return guestRead(t, sb, hostPath) },
			func() {
				if err := os.WriteFile(hostPath, []byte(want), 0o600); err != nil {
					t.Fatal(err)
				}
			})
		if !same {
			continue
		}
		res, err := hostSync(t, sb, model.WorktreeSyncOptions{Force: true})
		if err != nil {
			t.Fatalf("round %d: sync: %v", i, err)
		}
		if len(res.Updated) == 0 {
			t.Fatalf("round %d: the sync saw nothing to deliver for a same-length edit: %+v", i, res)
		}
		if got := guestRead(t, sb, hostPath) + "\n"; got != want {
			t.Fatalf("round %d: sync returned success and the guest still reads %q, want %q — "+
				"the silent-staleness class MGIT-192's settle exists to prevent", i, got, want)
		}
		verbRounds++
		i++
	}
	if verbRounds < rounds/2 {
		t.Fatalf("only %d verb rounds landed in one second; the machine is too slow for this measurement", verbRounds)
	}
	t.Logf("REAL VM PASS: %d same-second same-length host edits, each observed by the guest after the production sync", verbRounds)
}
