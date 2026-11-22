package reviewlaunch

import "testing"

func TestExecDecider_AllowsExactlyOneMatchingExecveat(t *testing.T) {
	const fd = 7
	d := NewExecDecider(fd, ArchAmd64)
	n := Syscalls(ArchAmd64)
	good := [6]uint64{fd, 0, 0, 0, AtEmptyPath, 0}

	if dec := d.Decide(n.Execveat, [6]uint64{fd + 1, 0, 0, 0, AtEmptyPath, 0}, true); dec.Allow {
		t.Error("execveat on another fd was allowed")
	}
	if dec := d.Decide(n.Execveat, [6]uint64{fd, 0, 0, 0, 0, 0}, true); dec.Allow {
		t.Error("execveat without AT_EMPTY_PATH was allowed")
	}
	if dec := d.Decide(n.Execve, good, true); dec.Allow {
		t.Error("a path execve reaching the supervisor was allowed")
	}
	if dec := d.Decide(n.Execveat, good, false); dec.Allow {
		t.Error("execveat with a non-empty path (which would ignore the fd) was allowed")
	}
	// The refused attempts above must not have used up the one exec.
	if dec := d.Decide(n.Execveat, good, true); !dec.Allow {
		t.Fatalf("first matching execveat refused: %+v", dec)
	}
	dec := d.Decide(n.Execveat, good, true)
	if dec.Allow || dec.Errno != 1 {
		t.Errorf("second matching execveat: %+v, want EPERM", dec)
	}
}
