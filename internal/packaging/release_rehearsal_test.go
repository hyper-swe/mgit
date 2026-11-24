package packaging

import (
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
	assert.Contains(t, gr, "--snapshot", "a rehearsal must build in snapshot mode, which publishes nothing")
	assert.Contains(t, gr, "inputs.rehearse_as", "the snapshot flag is chosen by the rehearsal input")
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

func TestReleaseRehearsal_TheWorkflowNeverMakesOrPushesATag(t *testing.T) {
	wf := releaseWorkflow(t)
	for _, banned := range []string{"git tag", "git push", "--tags", "gh release create", "git/refs"} {
		assert.NotContains(t, wf, banned, "the release workflow must not create or push refs")
	}
}
