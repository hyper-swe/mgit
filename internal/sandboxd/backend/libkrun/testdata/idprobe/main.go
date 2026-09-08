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
	fmt.Printf("uid=%d gid=%d name=%s home=%s home_file_owner=%s\n", uid, gid, name, home, owner)
}
