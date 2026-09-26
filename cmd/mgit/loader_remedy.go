package main

import (
	"debug/elf"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// linuxGlibcFloor is the oldest glibc the Linux release runs on: the bundle
// is built in ubuntu:20.04, and scripts/release/verify-linux-sandboxd.sh
// refuses a bundle that needs anything newer. A test keeps the two equal.
// Refs: MGIT-229, MGIT-230.4
const linuxGlibcFloor = "2.31"

// glibcTooOldRe matches glibc's loader refusing an object that needs a newer
// C library than the host has: `version `GLIBC_2.30' not found`.
var glibcTooOldRe = regexp.MustCompile("version [`']GLIBC_([0-9][0-9.]*)' not found")

// loaderRemedy reads what a daemon that could not start printed and returns
// the missing library ("" when none is missing) and the remedy for the cause
// it names ("" when it names none it knows).
//
// The platform comes from the loader's own words, not from the host running
// this: dyld's "Library not loaded" is macOS, where libkrun arrives through
// Homebrew, and ld.so's "error while loading shared libraries" is Linux, where
// the release archive carries it. A glibc VERSION refusal is not a missing
// library, and no reinstall fixes it, so it is named as what it is.
// Refs: MGIT-230.4, MGIT-61.15, MGIT-206
func loaderRemedy(output, daemonPath string) (lib, remedy string) {
	if m := glibcTooOldRe.FindStringSubmatch(output); m != nil {
		return "", glibcRemedy("GLIBC_" + m[1])
	}
	lib = missingLibrary(output)
	if lib == "" {
		return "", ""
	}
	goos := "linux"
	if strings.Contains(output, "Library not loaded: ") {
		goos = "darwin"
	}
	return lib, missingLibraryRemedy(lib, daemonPath, goos)
}

// glibcRemedy names an old C library as that, with the floor.
func glibcRemedy(needed string) string {
	return fmt.Sprintf("This host's C library is older than mgit-sandboxd needs: the loader could not find %s. "+
		"The Linux release is built for glibc %s or newer (Ubuntu 20.04, Debian 11 and later), so reinstalling "+
		"will not help: run the sandbox on a host with a newer glibc, or build the daemon on this one from source "+
		"(docs/INSTALL-SANDBOX.md). Core mgit is unaffected.", needed, linuxGlibcFloor)
}

// linuxLibraryRemedy says where the Linux release keeps the libkrun
// libraries, where THIS daemon's loader looked for them (its run path, read
// from the binary with $ORIGIN expanded), and that the fix is the archive.
// Refs: MGIT-230.4, MGIT-229
func linuxLibraryRemedy(lib, daemonPath string) string {
	if !strings.HasPrefix(lib, "libkrun") {
		return ""
	}
	var searched string
	switch dirs, err := runPathDirs(daemonPath); {
	case daemonPath == "":
		searched = ""
	case err != nil:
		searched = fmt.Sprintf("mgit could not read its run path (%v), so where it looked is unknown. ", err)
	case len(dirs) == 0:
		searched = daemonPath + " carries no run path, so its loader searched only the system's library path. "
	default:
		searched = fmt.Sprintf("Its loader searched %s, the run path of %s, and %s was not there. ",
			strings.Join(dirs, " and "), daemonPath, lib)
	}
	return "The Linux release ships libkrun and libkrunfw inside the archive, in lib/ beside mgit-sandboxd " +
		"(install.sh puts them in <prefix>/lib/mgit). " + searched +
		"Reinstall from the release archive: extract the whole archive, or run install.sh again, so the " +
		"libraries stay where the daemon looks. A daemon you built yourself with -tags libkrun finds libkrun " +
		"on the system's library path instead.\n"
}

// runPathDirs returns the directories in an ELF binary's DT_RPATH and
// DT_RUNPATH, in order, with $ORIGIN expanded to the directory the binary
// really lives in, which is the loader's meaning of it.
func runPathDirs(binary string) ([]string, error) {
	f, err := elf.Open(binary)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only; nothing to flush
	origin := filepath.Dir(binary)
	if real, evalErr := filepath.EvalSymlinks(binary); evalErr == nil {
		origin = filepath.Dir(real)
	}
	var dirs []string
	for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH} {
		vals, dynErr := f.DynString(tag)
		if dynErr != nil {
			return nil, dynErr
		}
		for _, v := range vals {
			for _, d := range strings.Split(v, ":") {
				d = strings.NewReplacer("${ORIGIN}", origin, "$ORIGIN", origin).Replace(d)
				if d != "" {
					dirs = append(dirs, filepath.Clean(d))
				}
			}
		}
	}
	return dirs, nil
}
