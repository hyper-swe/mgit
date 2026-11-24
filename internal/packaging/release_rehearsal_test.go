package packaging

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A REHEARSAL OF THE RELEASE THAT PUBLISHES NOTHING. The release workflow
// could only be exercised by pushing a tag, and a tag publishes: a GitHub
// release, a signature upload and an update of the public package tap. A gate
// that can only be tried by shipping is tried by shipping, so a dispatch with
// `rehearse_as` runs the same Preflight, e2e gate and GoReleaser in snapshot
// mode, and every step that writes outside the run is skipped. These tests
// read the workflow text, job by job, because a skipped step is a line.

const rehearsalOff = "inputs.rehearse_as == ''"

func releaseWorkflow(t *testing.T) string {
	t.Helper()
	return readRepoFile(t, ".github/workflows/release.yml")
}

func TestReleaseRehearsal_TheDispatchTakesAnOptionalTagShapedLabelAndSmokeStillNeedsATag(t *testing.T) {
	wf := releaseWorkflow(t)
	in := yamlBlock(t, wf, "workflow_dispatch:")
	require.Contains(t, in, "rehearse_as:", "the dispatch has no rehearse_as input")
	rehearse := yamlBlock(t, in, "rehearse_as:")
	assert.Contains(t, rehearse, "required: false")
	tag := yamlBlock(t, in, "tag:")
	assert.Contains(t, tag, "required: false", "a rehearsal has no published tag, so the tag input cannot be required by the dispatch")
	// With neither input a dispatch has nothing to do; it must fail loudly,
	// not smoke a branch name.
	assert.Contains(t, wf, "give either tag (smoke a published release) or rehearse_as (rehearse one)")
}

func TestReleaseRehearsal_TheLabelIsValidatedBeforeAnyStepUsesIt(t *testing.T) {
	pre := jobBlock(t, releaseWorkflow(t), "preflight")
	step := stepBlock(t, pre, "- name: Validate the rehearsal label")
	assert.Contains(t, step, "REHEARSE_AS: ${{ inputs.rehearse_as }}", "the input reaches the script through the environment, never pasted into it")
	assert.NotContains(t, strings.ReplaceAll(step, "REHEARSE_AS: ${{ inputs.rehearse_as }}", ""), "${{ inputs.rehearse_as }}")
	assert.Contains(t, step, `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)
	assert.Less(t, strings.Index(pre, "- name: Validate the rehearsal label"), strings.Index(pre, "- name: Verify Go modules"))
}

func TestReleaseRehearsal_PreflightRunsOnATagPushAndOnARehearsal(t *testing.T) {
	pre := jobBlock(t, releaseWorkflow(t), "preflight")
	assert.Contains(t, pre, "if: github.event_name == 'push' || inputs.rehearse_as != ''")
}

func TestReleaseRehearsal_GoreleaserRunsInSnapshotModeAndNothingPublishes(t *testing.T) {
	rel := jobBlock(t, releaseWorkflow(t), "release")
	gr := stepBlock(t, rel, "- name: Run GoReleaser")
	// The direction matters, not only the two strings: a rehearsal gets the
	// snapshot flag and a tag push gets the plain release. Reversing the two
	// would publish from a rehearsal, so the whole expression is pinned.
	assert.Contains(t, gr, "args: ${{ inputs.rehearse_as != '' && 'release --clean --snapshot' || 'release --clean' }}",
		"a rehearsal must build in snapshot mode, which publishes nothing, and a tag push must not")
	for _, name := range []string{"- name: Sign checksums", "- name: Upload signature", "- name: Upload build environment"} {
		assert.Contains(t, stepBlock(t, rel, name), rehearsalOff, "%s writes outside the run and must not run in a rehearsal", name)
	}
	// Every step that can write to a GitHub release is one of the above.
	assert.Equal(t, 2, strings.Count(rel, "softprops/action-gh-release"), "a new publishing step must be added to this test")
}

func TestReleaseRehearsal_TheSmokeJobsSmokeAPublishedReleaseOnly(t *testing.T) {
	wf := releaseWorkflow(t)
	for _, id := range []string{"release-smoke", "release-smoke-linux"} {
		job := jobBlock(t, wf, id)
		cond := yamlBlock(t, job, "if: always()")
		assert.Contains(t, cond, rehearsalOff, "%s would smoke an archive a rehearsal never published", id)
	}
}

func TestReleaseRehearsal_TheTapIsUpdatedOnlyByAPushOfAStableTag(t *testing.T) {
	hb := jobBlock(t, releaseWorkflow(t), "homebrew")
	assert.Contains(t, hb, "github.event_name == 'push'")
	assert.Contains(t, hb, "!contains(github.ref_name, '-')", "a pre-release tag (one with a hyphen) must not update the public tap")
}

// THE GUARANTEE IS "NO TAG IS PUSHED OR PUBLISHED", NOT "NO TAG EXISTS". The
// e2e gate the rehearsal calls makes a runner-local tag in its own throwaway
// checkout when it is not on a tag (its Linux user-path job), and never
// pushes it. So neither workflow may push a ref or create a release; the local
// tag is allowed only in e2e.yml.
func TestReleaseRehearsal_NeitherWorkflowPushesARefOrCreatesAReleaseAndOnlyE2eMakesALocalTag(t *testing.T) {
	rel, e2e := releaseWorkflow(t), readRepoFile(t, ".github/workflows/e2e.yml")
	for name, wf := range map[string]string{"release.yml": rel, "e2e.yml": e2e} {
		for _, banned := range []string{"git push", "--tags", "gh release create", "git/refs", "git tag -f", "push origin"} {
			assert.NotContains(t, wf, banned, "%s must not push a ref or create a release", name)
		}
	}
	assert.NotContains(t, rel, "git tag", "release.yml makes no tag, not even a local one")
}

// The label is validated as a WHOLE: a valid first line followed by a line
// break and anything else must be refused. The step's own script is run, not
// a copy of it.
func TestReleaseRehearsal_TheValidationStepRefusesAMultilineLabel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the step is a bash script")
	}
	pre := jobBlock(t, releaseWorkflow(t), "preflight")
	step := stepBlock(t, pre, "- name: Validate the rehearsal label")
	_, after, ok := strings.Cut(step, "run: |\n")
	require.True(t, ok, "the step has no run block")
	lines := strings.Split(after, "\n")
	script := make([]string, 0, len(lines))
	for _, l := range lines {
		script = append(script, strings.TrimPrefix(l, "          "))
	}
	for name, tc := range map[string]struct {
		label string
		ok    bool
	}{
		"empty is not a rehearsal":      {"", true},
		"a stable tag":                  {"v0.8.1", true},
		"a pre-release tag":             {"v0.8.1-dev1", true},
		"not a tag":                     {"main", false},
		"a trailing line break":         {"v0.8.1\n", false},
		"a valid line then another":     {"v0.8.1\nrm -rf x", false},
		"a valid line after an invalid": {"x\nv0.8.1", false},
		"a carriage return":             {"v0.8.1\r", false},
		"a space":                       {"v0.8.1 ", false},
	} {
		cmd := exec.Command("bash", "-c", strings.Join(script, "\n")) //nolint:gosec // G204: the script is the step text of this repository's own workflow, run to test it
		cmd.Env = append(cmd.Environ(), "REHEARSE_AS="+tc.label)
		err := cmd.Run()
		assert.Equal(t, tc.ok, err == nil, "%s (%q)", name, tc.label)
	}
}
