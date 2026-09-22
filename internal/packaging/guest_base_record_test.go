package packaging

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// THE SMOKE COMPOSES THE RECORD, NOT A TAG. A release records the guest base
// it was smoke-tested with, and the only way that record is true is that the
// smoke composes FROM it: the e2e leg and the two posture scripts default to
// `mgit sandbox base from` with no reference (the binary's embedded record)
// instead of a bare `debian:12` that resolves to whatever the tag points at
// that minute. Refs: MGIT-219, MGIT-218
func TestGuestBase_TheSmokeComposesTheReleaseRecordNotABareTag(t *testing.T) {
	wf := readRepoFile(t, filepath.Join(".github", "workflows", "e2e.yml"))
	assert.NotContains(t, wf, "MGIT_GUEST_OCI_REF: debian:12",
		"the e2e leg must compose the record, not pin a moving tag in the workflow")

	for _, script := range []string{"sandbox_posture.sh", "sandbox_cli_surface.sh"} {
		s := readRepoFile(t, filepath.Join("scripts", "e2e", script))
		assert.NotContains(t, s, ":-debian:12}", "%s: no bare-tag default — the record is the default", script)
		assert.Contains(t, s, "sandbox base from", "%s composes a base", script)
		assert.Contains(t, s, "release", "%s says it composes the release's recorded base when no image is given", script)
	}

	// The record itself is committed beside the code that embeds it.
	rec := readRepoFile(t, filepath.Join("internal", "sandboxd", "guestbase", "release-base.json"))
	assert.Regexp(t, `"digest":\s*"sha256:[0-9a-f]{64}"`, rec)
	assert.True(t, strings.Contains(rec, `"image"`), "the record names the image")
}

// The pin script and the preflight condition exist and are wired: the record
// is refreshed by one committed script, and the pre-tag check refuses a
// missing or unresolvable record. Refs: MGIT-219
func TestGuestBase_ThePinScriptAndThePreflightConditionExist(t *testing.T) {
	pin := readRepoFile(t, filepath.Join("scripts", "release", "pin-guest-base.sh"))
	assert.Contains(t, pin, "sandbox base resolve")
	assert.Contains(t, pin, "release-base.json")

	pre := readRepoFile(t, filepath.Join("scripts", "ci", "release-preflight.sh"))
	assert.Contains(t, pre, "release-base.json", "the preflight reads the record at the sha")
	assert.Contains(t, pre, "sandbox base resolve", "and proves the digest is still served")

	self := readRepoFile(t, filepath.Join("scripts", "ci", "release-preflight-selftest.sh"))
	assert.Contains(t, self, "release-base.json", "the self-test exercises the record condition")
}
