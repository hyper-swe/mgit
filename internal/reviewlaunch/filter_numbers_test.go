//go:build linux && (amd64 || arm64)

package reviewlaunch

import "testing"

// checkNumbers compares the hand-typed table with the platform's constants,
// by name, both ways: nothing typed is wrong, and nothing the platform names
// for this list is missing.
func checkNumbers(t *testing.T, a Arch, want map[string]uint32) {
	t.Helper()
	got := map[string]uint32{}
	for _, d := range WriteDeniedSyscalls(a) {
		got[d.Name] = d.Nr
	}
	for name, nr := range want {
		if name == "openat" || name == "open" {
			continue // the flags rule, checked through Syscalls
		}
		if g, ok := got[name]; !ok || g != nr {
			t.Errorf("%v %s: table has %d (present=%v), platform says %d", a, name, g, ok, nr)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%v %s is in the table but the platform check does not know it", a, name)
		}
	}
	n := Syscalls(a)
	if n.Ioctl != unixIoctl {
		t.Errorf("%v ioctl %d, platform %d", a, n.Ioctl, unixIoctl)
	}
	if w, ok := want["openat"]; !ok || n.Openat != w {
		t.Errorf("%v openat %d, platform %d", a, n.Openat, w)
	}
	if w, ok := want["open"]; ok && n.Open != w {
		t.Errorf("%v open %d, platform %d", a, n.Open, w)
	}
}
