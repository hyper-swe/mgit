package packaging

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The corresponding source of libkrunfw's bundled kernel is hosted by every
// mgit release, under the signed checksums, and for the macOS library built
// from upstream's prebuilt kernel image that source includes the exact
// prebuilt asset. The offer names that release and a contact address.
// Refs: MGIT-306

const libkrunfwPrebuiltSHA256 = "adef6739cc3bda2a1b57f04c08ccbfdf206a6c831d76b32de1beddc0e7984e86"

// The libkrunfw source archive is one file with one digest: the tap release
// libkrunfw-5.6.2 publishes it, and every mgit release attaches those exact
// bytes instead of building its own. Refs: MGIT-306
const libkrunfwSourceSHA256 = "df45d649fcbd07a4d0ca03fa836b2f640fdcf9b40187f3eb7023259c6e83d582"

const libkrunfwSourceContact = "swe@hyperswe.com"

func TestPins_RecordTheLibkrunfwPrebuiltAssetAndItsDigest(t *testing.T) {
	pins := readRepoFile(t, "scripts/sandbox-image/pins.env")
	assert.Contains(t, pins, "LIBKRUNFW_PREBUILT_SHA256="+libkrunfwPrebuiltSHA256)
	assert.Contains(t, pins, "LIBKRUNFW_PREBUILT_URL=https://github.com/containers/libkrunfw/releases/download/v5.6.2/libkrunfw-prebuilt-aarch64.tgz")
}

func TestE2EWorkflow_TheSourceAssetsHoldTheVerifiedPrebuiltKernelAsset(t *testing.T) {
	job := jobBlock(t, readRepoFile(t, ".github/workflows/e2e.yml"), "linux-sandboxd")
	for _, want := range []string{
		`echo "$LIBKRUNFW_PREBUILT_SHA256  `,
		`libkrunfw-$LIBKRUNFW_VERSION-prebuilt-aarch64.tgz`,
	} {
		assert.Contains(t, job, want, "the corresponding-source step must carry %q", want)
	}
}

func TestReleaseWorkflow_RequiresThePrebuiltAssetPublishedAndChecksummed(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release.yml")
	assert.Contains(t, wf, `"libkrunfw-$LIBKRUNFW_VERSION-prebuilt-aarch64.tgz"`,
		"the post-release check must require the prebuilt asset in the assets and in checksums.txt")
}

func TestSourceOffers_NameTheReleaseAssetsAndTheContact(t *testing.T) {
	for _, file := range []string{"install.sh", "scripts/release/build-linux-sandboxd.sh"} {
		body := readRepoFile(t, file)
		assert.Contains(t, body, libkrunfwSourceContact, "%s: the source offer must name its contact", file)
		assert.Contains(t, body, "prebuilt-aarch64.tgz", "%s: the source offer must list the prebuilt asset", file)
	}
}

func TestPins_RecordTheOneLibkrunfwSourceArchiveAndItsDigest(t *testing.T) {
	pins := readRepoFile(t, "scripts/sandbox-image/pins.env")
	assert.Contains(t, pins, "LIBKRUNFW_SOURCE_SHA256="+libkrunfwSourceSHA256)
	assert.Contains(t, pins, "LIBKRUNFW_SOURCE_URL=https://github.com/hyper-swe/homebrew-tap/releases/download/libkrunfw-5.6.2/libkrunfw-v5.6.2-source.tar.gz")
}

func TestE2EWorkflow_TheLibkrunfwSourceIsTheTapReleasesFileCheckedAgainstItsPin(t *testing.T) {
	job := jobBlock(t, readRepoFile(t, ".github/workflows/e2e.yml"), "linux-sandboxd")
	assert.NotContains(t, job, `libkrun-src/libkrunfw" archive`,
		"the libkrunfw source archive must not be rebuilt with git archive: a second file with a second digest confuses a recipient")
	for _, want := range []string{
		`curl -fsSL -o "$libkrunfw_src" "$LIBKRUNFW_SOURCE_URL"`,
		`echo "$LIBKRUNFW_SOURCE_SHA256  $libkrunfw_src" | sha256sum -c -`,
		`cp "$libkrunfw_src" "$src/libkrunfw-$LIBKRUNFW_VERSION-source.tar.gz"`,
	} {
		assert.Contains(t, job, want, "the corresponding-source step must carry %q", want)
	}
}

func TestReleaseWorkflow_RequiresThePublishedLibkrunfwSourceToCarryThePinnedDigest(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release.yml")
	assert.Contains(t, wf, `"^$LIBKRUNFW_SOURCE_SHA256  libkrunfw-$LIBKRUNFW_VERSION-source.tar.gz\$"`,
		"the post-release check must require checksums.txt to list the libkrunfw source archive at the pinned digest")
}

// libkrunfw v5.6.2's Makefile installs into lib/<arch>-linux-gnu on a Debian
// family system (v5.5.0 used lib64), so the Linux build, which looks in
// $prefix/lib64, found no libkrunfw.so. The install step names the directory
// itself, and the script checks the library landed there before it reports
// success. Refs: MGIT-306.2
func TestBuildLibkrun_LibkrunfwInstallsIntoLib64AndIsChecked(t *testing.T) {
	sh := readRepoFile(t, "scripts/sandbox-image/build-libkrun.sh")
	assert.Contains(t, sh, `make PREFIX="$prefix" LIBDIR_Linux=lib64 install`,
		"libkrunfw's install must be told the lib64 directory the later steps read")
	assert.Contains(t, sh, `"$libdir"/libkrunfw.so.*`,
		"the script must check libkrunfw is under the libdir, not only libkrun")
}
