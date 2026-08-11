// Command fsprobe answers, from inside a real guest, what that guest can
// actually see of a shared directory.
//
// It exists because a host-side assertion about the share proves nothing about
// the guest's view — the two genuinely disagree on some backends, which is the
// whole subject of MGIT-90 — and the minimal guest bases these e2e tests boot
// carry no shell or coreutils to ask with. Refs: MGIT-90, MGIT-76
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: fsprobe <stat|read|ls> <path>")
		os.Exit(2)
	}
	path := os.Args[2]
	switch os.Args[1] {
	case "stat":
		if fi, err := os.Stat(path); err != nil {
			fmt.Printf("STAT %s -> ABSENT\n", path)
		} else {
			fmt.Printf("STAT %s -> size=%d\n", path, fi.Size())
		}
	case "read":
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("READ %s -> ABSENT\n", path)
			return
		}
		fmt.Printf("READ %s -> %q\n", path, string(b))
	case "ls":
		ents, err := os.ReadDir(path)
		if err != nil {
			fmt.Printf("LS %s -> ERROR %v\n", path, err)
			return
		}
		for _, e := range ents {
			fmt.Printf("ENTRY %s\n", e.Name())
		}
	default:
		os.Exit(2)
	}
}
