package libkrun

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// THE DAEMON'S OWN LIBRARY DIRECTORY IS SEARCHED FOR LIBKRUNFW.
//
// libkrun loads libkrunfw by its file name, so the VM child finds it only in
// the directories its loader path names. install.sh's opt-in step puts
// libkrunfw in <prefix>/lib/mgit, beside the libkrun the release ships, and an
// extracted archive keeps its libraries in lib/ beside the daemon. Neither was
// on the list, so a libkrunfw placed exactly where the installer puts it was
// never found. The daemon's real executable is used, so the test exercises
// the same lookup a running daemon makes. Refs: MGIT-306

func libkrunfwTestName() string {
	if runtime.GOOS == "darwin" {
		return "libkrunfw.5.dylib"
	}
	return "libkrunfw.so.5"
}

func TestLibkrunfwDirs_FindsLibkrunfwInTheDaemonsOwnLibDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve the test binary: %v", err)
	}
	if real, evalErr := filepath.EvalSymlinks(exe); evalErr == nil {
		exe = real
	}
	tests := []struct {
		name    string
		created string // the directory this case creates, removed afterwards
		libDir  string // where libkrunfw is placed, relative to the binary's directory
	}{
		{name: "archive_layout_lib_beside_the_daemon", created: "lib", libDir: "lib"},
		{name: "installed_layout_prefix_lib_mgit", created: filepath.Join("..", "lib"), libDir: filepath.Join("..", "lib", "mgit")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := filepath.Dir(exe)
			created := filepath.Join(base, tt.created)
			if _, statErr := os.Stat(created); statErr == nil {
				t.Fatalf("%s already exists beside the test binary; refusing to change it", created)
			}
			t.Cleanup(func() { _ = os.RemoveAll(created) }) // this test created it
			dir := filepath.Clean(filepath.Join(base, tt.libDir))
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, libkrunfwTestName()), []byte("lib"), 0o600); err != nil {
				t.Fatal(err)
			}

			got := libkrunfwDirs()

			// The daemon lists its path as given first, then as resolved; a
			// temp directory behind a symlink (macOS /var) makes them differ,
			// so the first entry must be the same directory, not the same text.
			same := len(got) > 0 && got[0] == dir
			if len(got) > 0 && !same {
				a, errA := filepath.EvalSymlinks(got[0])
				b, errB := filepath.EvalSymlinks(dir)
				same = errA == nil && errB == nil && a == b
			}
			if !same {
				t.Errorf("libkrunfwDirs() = %v, want %q first: the libkrunfw beside this daemon "+
					"belongs to this install and must win over a system copy", got, dir)
			}
		})
	}
}
