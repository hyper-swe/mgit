// idprobe is the guest-side witness for MGIT-151: run inside a guest as
// whatever identity the supervisor gave it, it prints its OWN view — the
// uid and gid it runs as, the name the guest's passwd resolves for that uid,
// its HOME, and the owner of a file it creates there — so a test asserts
// what the child experienced, never what the host claimed. One static
// binary for both backends. Refs: MGIT-151
package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
)

func main() {
	uid, gid := os.Getuid(), os.Getgid()
	name := "?"
	if u, err := user.LookupId(fmt.Sprint(uid)); err == nil {
		name = u.Username
	}
	home := os.Getenv("HOME")
	owner := "none"
	probe := filepath.Join(home, ".idprobe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		var st syscall.Stat_t
		if err := syscall.Stat(probe, &st); err == nil {
			owner = fmt.Sprintf("%d:%d", st.Uid, st.Gid)
		}
	} else {
		owner = "unwritable:" + err.Error()
	}
	line := fmt.Sprintf("uid=%d gid=%d name=%s home=%s home_file_owner=%s", uid, gid, name, home, owner)
	// With a directory argument — the worktree, on a backend whose guest
	// ships no `stat` — it also writes a file there and reports its owner.
	if len(os.Args) > 1 {
		line += " dir_file_owner=" + ownerOfNewFile(filepath.Join(os.Args[1], ".idprobe-owned"))
	}
	fmt.Println(line)
}

// ownerOfNewFile creates path and returns "uid:gid" of what was created,
// or the error that stopped it.
func ownerOfNewFile(path string) string {
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		return "unwritable:" + err.Error()
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "unstattable:" + err.Error()
	}
	return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
}
