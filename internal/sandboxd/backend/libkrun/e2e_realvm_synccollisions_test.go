//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
	"github.com/hyper-swe/mgit/internal/sandboxd/worktreesync"
)

// The REMAINING collision classes, through a live virtiofs mount.
//
// WHY THIS FILE EXISTS. ADR-011's collision policy has three ways a sync can
// be REFUSED, and e2e_realvm_sync_test.go boots a real VM for exactly one of
// them (host changed a path the guest also modified). The other two were
// covered only by worktreesync's unit tests, which write into a plain host
// directory with no VM anywhere — the same blind spot that let host->guest
// delivery ship broken in MGIT-71: the host tree is right and the guest still
// reads something else.
//
// The two classes here are not exotic. "Host added a file the guest already
// created" is what happens the first time an agent's build writes a generated
// file the human then also writes; "host deleted a path the guest changed" is
// a refactor landing while the sandbox has un-landed edits to the removed
// file. Both are silent data loss if the refusal ever stops firing, and a
// backend-specific regression (a staged tree the guest does not really mount,
// a manifest that stops recording deliveries) would make BOTH stop firing
// while every host-side unit test kept passing.
//
// Each test pairs its refusal with a POSITIVE CONTROL on the same sandbox and
// the same path: a sync that is simply broken also refuses everything, so a
// refusal alone is not evidence of a decision.
//
// Refs: MGIT-87, MGIT-76, MGIT-71, ADR-011

// guestReadStatus reads a path through the guest and reports the reader's exit
// code instead of failing on it, so a test can assert that a path is GONE from
// the guest rather than merely empty. Refs: MGIT-87
func guestReadStatus(t *testing.T, s syncSandbox, path string) (stdout string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := newGuestDialer(s.workDir).DialGuest(ctx, s.id)
	if err != nil {
		t.Fatalf("host could not reach the guest exec channel: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var out, errBuf bytes.Buffer
	res, err := guestexec.Run(conn, model.ExecRequest{Command: []string{"/bin/cat", path}}, &out, &errBuf)
	if err != nil {
		t.Fatalf("guest cat %s: transport failure: %v (stderr=%q)", path, err, errBuf.String())
	}
	return strings.TrimSpace(out.String()), res.ExitCode
}

// TestE2E_Libkrun_RealVM_Sync_RefusesAHostAddOverAGuestCreatedFile is collision
// class 2: the host adds a path that was NEVER delivered, and the guest already
// has a file of that name. Honoring the host would destroy a file the sandbox
// created and nobody has landed.
//
// It also asserts the non-conflicting twin that shares the code path, because
// they are one decision made twice: a guest-created file the host does NOT
// have must SURVIVE the sync untouched. That is what keeps node_modules and
// build caches alive across rounds, and a policy that deleted them would look
// identical from the host. Refs: MGIT-87, MGIT-76, ADR-011
func TestE2E_Libkrun_RealVM_Sync_RefusesAHostAddOverAGuestCreatedFile(t *testing.T) {
	requireRealVM(t)

	sb := launchRealVMForSync(t, "syncadd", "MGIT-76")

	// The guest creates two files of its own. Writing into the staged tree is
	// how the guest's own writes appear (it mounts that directory read-write);
	// the guest reads below confirm it observes exactly these.
	if err := os.WriteFile(filepath.Join(sb.staged, "generated.txt"), []byte("GUEST BUILT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb.staged, "cache.bin"), []byte("GUEST CACHE"), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(sb.worktree, "generated.txt")
	cache := filepath.Join(sb.worktree, "cache.bin")
	if got := guestRead(t, sb, generated); got != "GUEST BUILT" {
		t.Fatalf("precondition: the guest must see its own file, got %q (a failure "+
			"here means the share is not live and the rest proves nothing)", got)
	}

	// The host now adds a file of the same name. This is the collision.
	if err := os.WriteFile(generated, []byte("HOST WROTE"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := hostSync(t, sb, model.WorktreeSyncOptions{})
	if err == nil {
		t.Fatal("a host add over a guest-created file must refuse; overwriting it " +
			"is destruction of work nobody has landed")
	}
	if !errors.Is(err, worktreesync.ErrConflict) {
		t.Fatalf("the refusal must be classifiable as a conflict, not generic failure: %v", err)
	}
	if report == nil || !report.Refused || len(report.Conflicts) != 1 {
		t.Fatalf("a refusal must carry exactly the classification that caused it, got %+v", report)
	}
	if report.Conflicts[0].Path != "generated.txt" {
		t.Fatalf("the refusal must name the conflicting path, got %+v", report.Conflicts)
	}
	// The REASON is asserted, not just the refusal: created-in-guest and
	// modified-in-guest are different remedies for the operator, and a
	// misclassification would send them at the wrong one.
	if report.Conflicts[0].Reason != string(worktreesync.ReasonCreatedInGuest) {
		t.Errorf("reason = %q, want %q — the host never delivered this path, so "+
			"the guest created it", report.Conflicts[0].Reason, worktreesync.ReasonCreatedInGuest)
	}
	if got := guestRead(t, sb, generated); got != "GUEST BUILT" {
		t.Fatalf("a refused sync must leave the guest untouched, got %q", got)
	}

	// POSITIVE CONTROL, through the remedy the refusal itself names: --force
	// overwrites the guest's copy and reports it destroyed. Anything else would
	// be the test inventing a resolution no operator is offered.
	res, err := hostSync(t, sb, model.WorktreeSyncOptions{Force: true})
	if err != nil {
		t.Fatalf("positive control failed — --force refuses too, so the refusal "+
			"above proved nothing: %v", err)
	}
	if len(res.Overridden) != 1 || res.Overridden[0] != "generated.txt" {
		t.Fatalf("--force must report every guest path it destroyed; destroying "+
			"un-landed work unrecorded is the failure the policy exists to "+
			"prevent, got %+v", res)
	}
	if got := guestRead(t, sb, generated); got != "HOST WROTE" {
		t.Fatalf("guest must read the host's file after a forced sync, got %q", got)
	}

	// THE OTHER HALF of the same policy: a guest-created file the host never
	// had is never touched, not even by a sync that did deliver other work.
	if got := guestRead(t, sb, cache); got != "GUEST CACHE" {
		t.Fatalf("a guest-created path the host does not have must survive a sync "+
			"untouched (this is what keeps node_modules alive between rounds), got %q", got)
	}
	t.Log("REAL VM PASS: host-add over a guest-created file refused as created-in-guest, " +
		"then delivered by --force with the destroyed path reported; the guest's own " +
		"untouched file survived")
}

// TestE2E_Libkrun_RealVM_Sync_RefusesADeleteOfAPathTheGuestChanged is collision
// class 3: the host DELETES a delivered path the guest has since modified.
// Deletion is as destructive as an overwrite, and it travels a different code
// path from the update classes above (worktreesync.appendDeletes), so a
// regression could remove this refusal alone.
//
// Its positive control is the delete that SHOULD happen: once the guest's copy
// matches what was delivered, the same host deletion removes it from the guest.
// Refs: MGIT-87, MGIT-76, ADR-011
func TestE2E_Libkrun_RealVM_Sync_RefusesADeleteOfAPathTheGuestChanged(t *testing.T) {
	requireRealVM(t)

	sb := launchRealVMForSync(t, "syncdel", "MGIT-76")
	appPath := filepath.Join(sb.worktree, "seed.txt")

	// seed.txt was delivered by the launch; the guest now edits it.
	if err := os.WriteFile(filepath.Join(sb.staged, "seed.txt"), []byte("GUEST EDIT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := guestRead(t, sb, appPath); got != "GUEST EDIT" {
		t.Fatalf("precondition: the guest must observe its own edit, got %q", got)
	}

	// The host removes the path entirely.
	if err := os.Remove(appPath); err != nil {
		t.Fatal(err)
	}

	report, err := hostSync(t, sb, model.WorktreeSyncOptions{})
	if err == nil {
		t.Fatal("deleting a path the guest changed must refuse; a delete is as " +
			"destructive as an overwrite and loses the same un-landed work")
	}
	if !errors.Is(err, worktreesync.ErrConflict) {
		t.Fatalf("the refusal must be classifiable as a conflict: %v", err)
	}
	if report == nil || !report.Refused || len(report.Conflicts) != 1 ||
		report.Conflicts[0].Path != "seed.txt" {
		t.Fatalf("the refusal must name the path it protected, got %+v", report)
	}
	if report.Conflicts[0].Reason != string(worktreesync.ReasonModifiedInGuest) {
		t.Errorf("reason = %q, want %q", report.Conflicts[0].Reason, worktreesync.ReasonModifiedInGuest)
	}
	// Nothing was deleted: the guest still reads its edit.
	if got := guestRead(t, sb, appPath); got != "GUEST EDIT" {
		t.Fatalf("a refused delete must leave the guest's file exactly as it was, got %q", got)
	}
	t.Logf("REAL VM: host delete refused, naming %q: %v", report.Conflicts[0].Path, err)

	// POSITIVE CONTROL: the guest's copy stops diverging from what was
	// delivered, and the same host deletion now applies.
	if err := os.WriteFile(filepath.Join(sb.staged, "seed.txt"), []byte("host work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := hostSync(t, sb, model.WorktreeSyncOptions{})
	if err != nil {
		t.Fatalf("positive control failed — the delete refuses even unmodified, so "+
			"the refusal above proved nothing: %v", err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != "seed.txt" {
		t.Fatalf("the applied sync must report the delete it performed, got %+v", res)
	}
	// THE ASSERTION: the guest, not the host, no longer has the file. A
	// host-side stat would pass even if the guest still held the old inode.
	out, code := guestReadStatus(t, sb, appPath)
	if code == 0 {
		t.Fatalf("the guest can still read %s (%q) after a delete the sync reported "+
			"as applied — the host tree is right and the guest is stale", appPath, out)
	}
	t.Logf("REAL VM PASS: delete over a guest edit refused, then applied once resolved; "+
		"the guest's reader now exits %d for the removed path", code)
}
