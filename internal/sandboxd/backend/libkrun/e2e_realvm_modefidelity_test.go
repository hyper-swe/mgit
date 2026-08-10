//go:build cgo && !vzf && (darwin || (linux && libkrun))

package libkrun

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Mode fidelity across the virtio-fs share (MGIT-81).
//
// MGIT-73's export e2e measured that a guest-written 0755 reaches the HOST as
// 0600 and — correctly — refused to invent a mode it had not observed. What it
// could not say was WHERE the bits went: the guest's umask, the way the
// workload writes the file, or libkrun's virtio-fs. This test settles that by
// measurement inside one real boot:
//
//	guest tmpfs (control) -> guest share (subject) -> host view of the share
//
// The control arm runs the identical cases on a guest-private tmpfs that no
// virtio-fs code touches, which exonerates the guest and its umask. The host
// arm then proves the finding the export depends on: libkrun's macOS
// filesystem device presents guest-created inodes with PLACEHOLDER permission
// bits (0600 files, 0700 directories) and records the real st_mode in the
// user.containers.override_stat attribute, so the mode is fully observable
// host-side without the guest participating in anything.
// Refs: MGIT-81, MGIT-73, ADR-011

// recordedStatXattr is the attribute libkrun's macOS filesystem device writes
// for a guest-created inode, holding "<uid>:<gid>:<octal st_mode>". Named here
// independently of the export's own constant so this measurement would still
// catch the export quietly changing which attribute it trusts.
const recordedStatXattr = "user.containers.override_stat"

// guestModeLine is one parsed "GUEST-MODE <arm> <case> want=X got=Y" console
// report from the in-guest probe.
type guestModeLine struct {
	arm  string
	name string
	want fs.FileMode
	got  fs.FileMode
}

// parseGuestModes extracts the probe's report from the guest console.
func parseGuestModes(console string) map[string]guestModeLine {
	out := map[string]guestModeLine{}
	for _, raw := range strings.Split(console, "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) != 5 || fields[0] != "GUEST-MODE" {
			continue
		}
		var want, got uint32
		if _, err := fmt.Sscanf(fields[3], "want=%o", &want); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(fields[4], "got=%o", &got); err != nil {
			continue
		}
		out[fields[1]+"/"+fields[2]] = guestModeLine{arm: fields[1], name: fields[2],
			want: fs.FileMode(want), got: fs.FileMode(got)}
	}
	return out
}

// hostRecordedMode reads the permission bits the share recorded for a path,
// reporting false when there is no record.
func hostRecordedMode(t *testing.T, path string) (fs.FileMode, bool) {
	t.Helper()
	buf := make([]byte, 128)
	n, err := unix.Lgetxattr(path, recordedStatXattr, buf)
	if err != nil || n <= 0 {
		return 0, false
	}
	fields := strings.Split(string(buf[:n]), ":")
	if len(fields) != 3 {
		return 0, false
	}
	mode, perr := strconv.ParseUint(fields[2], 8, 32)
	if perr != nil {
		return 0, false
	}
	return fs.FileMode(mode) & fs.ModePerm, true
}

// TestE2E_Libkrun_RealVM_ModeFidelity_HostCanObserveTheModeTheGuestSet is the
// measurement MGIT-81 rests on. It fails if the guest starts losing modes of
// its own accord (a different bug), or if the host loses its ability to
// observe them — in which case the export's mode fidelity is gone and the
// documented limitation has to be rewritten from a fresh measurement.
// Refs: MGIT-81
func TestE2E_Libkrun_RealVM_ModeFidelity_HostCanObserveTheModeTheGuestSet(t *testing.T) {
	requireRealVM(t)
	repoRoot, worktree := exportFixture(t)
	_, _, staged := launchExportSandbox(t, repoRoot, worktree, exportGuestBase(t, worktree))

	console, err := os.ReadFile(filepath.Join(filepath.Dir(staged), consoleLogName)) //nolint:gosec // test-owned state dir
	if err != nil {
		t.Fatalf("read the guest console: %v", err)
	}
	modes := parseGuestModes(string(console))
	if len(modes) == 0 {
		t.Fatalf("the guest probe reported no modes; console:\n%s", console)
	}

	t.Log("case          guest wanted  tmpfs saw  share saw  host stat  host record")
	for _, name := range []string{"create-0755", "create-0644", "create-0600",
		"create-0777", "chmod-0755", "dir-0755"} {
		ctl, okCtl := modes["tmpfs/"+name]
		shr, okShr := modes["share/"+name]
		if !okCtl || !okShr {
			t.Fatalf("the probe did not report %q on both arms; console:\n%s", name, console)
		}
		info, err := os.Lstat(filepath.Join(staged, "modeprobe", name))
		if err != nil {
			t.Fatalf("host Lstat of the shared %s: %v", name, err)
		}
		host := info.Mode().Perm()
		recorded, hasRecord := hostRecordedMode(t, filepath.Join(staged, "modeprobe", name))
		t.Logf("%-13s %04o          %04o       %04o       %04o       %04o (present=%v)",
			name, shr.want, ctl.got, shr.got, host, recorded, hasRecord)

		// The control arm proves the guest is not the lossy party.
		if ctl.got != ctl.want {
			t.Errorf("%s: the guest lost the mode on its OWN tmpfs (wanted %04o, saw %04o) — "+
				"the umask or the workload, not the share", name, ctl.want, ctl.got)
		}
		// The guest's view of the share must match its view of a plain
		// filesystem: whatever the host mapping does, the guest is unaffected.
		if shr.got != shr.want {
			t.Errorf("%s: the guest sees %04o on the share, wanted %04o", name, shr.got, shr.want)
		}
		// The load-bearing one: the host can observe the guest's mode by ONE of
		// the two honest routes — the file's own bits, or the share's record.
		if host != shr.got && (!hasRecord || recorded != shr.got) {
			t.Errorf("%s: the guest set %04o but the host can observe neither that mode in the "+
				"file's bits (%04o) nor in a share record (present=%v, %04o) — an export can no "+
				"longer reproduce it honestly", name, shr.got, host, hasRecord, recorded)
		}
	}
}
