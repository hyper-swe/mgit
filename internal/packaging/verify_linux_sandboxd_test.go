package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bundle verifier ends with a NEGATIVE CONTROL: it removes libkrunfw and
// requires the daemon's --vmm to name the problem, because a check that
// cannot fail proves nothing. But the verifier itself runs only in the CI
// jobs that build the bundle, and their passing runs cannot show that its own
// failure branch works. Forcing that branch to pass left every test green.
// So the verifier runs here against a fixture bundle. Fake binutils
// (READELF/NM/OBJDUMP) print what a correct bundle's would, so the fixture
// reaches the control on any host. A daemon that notices a missing libkrunfw
// is the positive control: the whole verifier passes. A daemon that ignores
// it must make the verifier FAIL, naming the control. Refs: MGIT-230.9
func TestVerifyLinuxSandboxd_TheNegativeControlCanFail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Fatal("the verifier is a bash script run on Linux release builders; this test needs a POSIX shell")
	}
	tools := fakeBinutils(t)
	run := func(t *testing.T, daemonNotices bool) (string, error) {
		t.Helper()
		dir := fixtureBundle(t, daemonNotices)
		//nolint:gosec,noctx // G204: the repository's own verifier against a test fixture
		cmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "release", "verify-linux-sandboxd.sh"), dir)
		cmd.Env = append(os.Environ(),
			"READELF="+filepath.Join(tools, "readelf"),
			"NM="+filepath.Join(tools, "nm"),
			"OBJDUMP="+filepath.Join(tools, "objdump"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run(t, true)
	require.NoError(t, err, "positive control: a daemon that names a missing libkrunfw passes the whole verifier:\n%s", out)
	assert.Contains(t, out, "negative control: with libkrunfw.so.5 removed, --vmm names the problem")

	out, err = run(t, false)
	require.Error(t, err, "a daemon that ignores a missing libkrunfw must FAIL the verifier:\n%s", out)
	assert.Contains(t, out, "negative control did not fire")
}

// fixtureBundle lays out a bundle as the release archive does: the daemon
// with lib/ beside it. The daemon is a script answering --version and --vmm;
// when noticesMissing is true it reports libkrunfw's absence as a problem, as
// the real probe does, and otherwise it reports the library as resolved
// whether or not the file is there.
func fixtureBundle(t *testing.T, noticesMissing bool) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib"), 0o750))
	for _, lib := range []string{"libkrun.so.1", "libkrunfw.so.5"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", lib), []byte("fixture "+lib+"\n"), 0o600))
	}
	notice := ""
	if noticesMissing {
		notice = `[ -f "$lib/libkrunfw.so.5" ] || { echo '{"vmm":"libkrun","problems":["libkrun did not load libkrunfw"]}'; exit 0; }`
	}
	daemon := strings.Join([]string{
		"#!/bin/sh",
		`lib="$(cd "$(dirname "$0")/lib" && pwd -P)"`,
		`case "$1" in`,
		`--version) echo "mgit-sandboxd version 0.0.0-fixture" ;;`,
		`--vmm)`,
		notice,
		`  echo "{\"vmm\":\"libkrun\",\"libraries\":[{\"name\":\"libkrun\",\"path\":\"$lib/libkrun.so.1\"},{\"name\":\"libkrunfw\",\"path\":\"$lib/libkrunfw.so.5\"}]}" ;;`,
		`*) exit 2 ;;`,
		`esac`,
	}, "\n") + "\n"
	//nolint:gosec // G306: the fixture daemon must be executable
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mgit-sandboxd"), []byte(daemon), 0o750))
	return dir
}

// fakeBinutils writes readelf, nm and objdump stand-ins that describe a
// correct bundle: the daemon NEEDS libkrun with DT_RPATH $ORIGIN/lib and
// $ORIGIN/../lib/mgit, libkrun has DT_RPATH $ORIGIN and exports
// krun_add_net_unixgram, and nothing needs a glibc newer than 2.17.
func fakeBinutils(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	scripts := map[string]string{
		"readelf": `case "$(basename "$2")" in
mgit-sandboxd) printf ' 0x1 (NEEDED)  Shared library: [libkrun.so.1]\n 0xf (RPATH)  Library rpath: [$ORIGIN/lib:$ORIGIN/../lib/mgit]\n' ;;
libkrun.so.*) printf ' 0xf (RPATH)  Library rpath: [$ORIGIN]\n' ;;
esac`,
		"nm":      `echo "0000000000001000 T krun_add_net_unixgram"`,
		"objdump": `echo "0000000000000000      DF *UND*  0000000000000000  GLIBC_2.17  memcpy"`,
	}
	for name, body := range scripts {
		//nolint:gosec // G306: a stand-in tool must be executable
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o750))
	}
	return dir
}
