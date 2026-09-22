package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	relName = "registry-1.docker.io/library/debian:12"
	relDig  = "sha256:9a2bafc2cebc397fc253a4b80c6d4bc425eb5a2a819cd25ada003c7facd48f1d"
	movedD  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	otherN  = "registry-1.docker.io/library/golang:1.26-bookworm"
	otherD  = "sha256:37a6d96e0000000000000000000000000000000000000000000000000000aaaa"
	baseD   = "sha256:9e61d29dee097844faa45bdc5777fee78d0452b061684458c900b5a79b4fa50a"
)

// THE COMPOSED DIGEST BESIDE THE RELEASE'S RECORDED DIGEST, AND A MISMATCH
// STATED AS A DIFFERENCE — never "ok" over it. Two hosts recomposed from one
// moving tag ran different guest userspaces under the same ok row
// (MGIT-218); the release now records the base it was smoke-tested with,
// and this row compares. Refs: MGIT-219, R-H300 rule 2
func TestBaseReleaseCheck_StatesADifferenceAndNeverOkOverOne(t *testing.T) {
	tests := []struct {
		name       string
		id         BaseIdentity
		err        error
		wantStatus Status
		wantIn     []string
		wantNotIn  []string
	}{
		{
			name:       "same_image_same_digest_is_ok_and_names_both",
			id:         BaseIdentity{SourceRef: relName + "@" + relDig, BaseDigest: baseD, ReleaseRef: relName + "@" + relDig},
			wantStatus: StatusOK, wantIn: []string{relName, relDig, "smoke-tested"},
		},
		{
			name:       "same_name_different_digest_is_a_DIFFERENCE_naming_both_digests",
			id:         BaseIdentity{Composed: "0.6.8", SourceRef: relName + "@" + movedD, BaseDigest: baseD, ReleaseRef: relName + "@" + relDig},
			wantStatus: StatusDiffers, wantIn: []string{movedD, relDig, "not the one this release was tested with"},
		},
		{
			name:       "different_image_name_is_a_DIFFERENCE_that_says_the_name_differs",
			id:         BaseIdentity{Composed: "dev", SourceRef: otherN + "@" + otherD, BaseDigest: baseD, ReleaseRef: relName + "@" + relDig},
			wantStatus: StatusDiffers, wantIn: []string{otherN, otherD, relName, relDig, "different image"},
		},
		{
			name:       "a_directory_base_was_composed_from_no_image_at_all",
			id:         BaseIdentity{Composed: "0.6.8", BaseDigest: baseD, ReleaseRef: relName + "@" + relDig},
			wantStatus: StatusDiffers, wantIn: []string{"directory", relName, relDig},
		},
		{
			name:       "a_build_with_no_record_cannot_compare_and_says_so",
			id:         BaseIdentity{SourceRef: relName + "@" + relDig, BaseDigest: baseD},
			wantStatus: StatusNotChecked, wantIn: []string{"records no release guest base"},
		},
		{
			name:       "an_uninspectable_base_is_not_checked",
			err:        errors.New("no guest base registered for this repository"),
			wantStatus: StatusNotChecked, wantIn: []string{"no guest base registered"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BaseReleaseCheck{Inspect: func() (BaseIdentity, error) { return tt.id, tt.err }}.Run(context.Background())
			assert.Equal(t, tt.wantStatus, got.Status, "summary: %s", got.Summary)
			for _, w := range tt.wantIn {
				assert.Contains(t, got.Summary+" "+got.Reason, w)
			}
			assert.Equal(t, "MGIT-219", got.Incident)
			if tt.wantStatus == StatusDiffers {
				assert.Contains(t, got.Remedy, "sandbox base from", "the remedy names the recompose path")
				assert.Contains(t, got.Remedy, "knowingly", "and the other path: keeping the different image on purpose")
			} else {
				assert.Empty(t, got.Remedy, "an ok or not-checked row carries no remedy — a remedy on an ok row reads as a problem")
			}
		})
	}
}

// A stated difference is its own verdict: rendered as DIFF with its remedy,
// never as ok, and not a failure of the exit code — the reader decides.
func TestRender_ADifferenceIsNeitherOkNorFail(t *testing.T) {
	r := Result{Name: "base/release", Status: StatusDiffers, Summary: "differs", Remedy: "recompose", Incident: "MGIT-219"}
	out := Render([]Result{r})
	assert.True(t, strings.HasPrefix(out, "DIFF  base/release"), "the marker must not read as ok or FAIL: %q", out)
	assert.Contains(t, out, "remedy: recompose")
	assert.Contains(t, out, "DIFF", "the closing note tells the reader what a DIFF is")
	assert.False(t, Failed([]Result{r}), "a stated difference does not flip the exit code; it is read")
}

// A base composed by an mgit before 0.6.8 recorded the platform manifest's
// digest, not the image index the record pins: its digest differs from the
// record's even when nothing moved. The row says it cannot compare and asks
// for a recompose — never a false DIFF, never an ok. Refs: MGIT-219
func TestBaseReleaseCheck_ABaseComposedBeforeIndexPinning_IsNotComparableNotDifferent(t *testing.T) {
	old := BaseIdentity{Composed: "0.6.7", Running: "0.6.8", SourceRef: relName + "@" + movedD, BaseDigest: baseD, ReleaseRef: relName + "@" + relDig}
	got := BaseReleaseCheck{Inspect: func() (BaseIdentity, error) { return old, nil }}.Run(context.Background())
	assert.Equal(t, StatusNotChecked, got.Status, got.Summary)
	assert.Contains(t, got.Summary+" "+got.Reason, "0.6.7")
	assert.Contains(t, got.Summary+" "+got.Reason, "platform manifest")
	assert.Contains(t, got.Summary, "sandbox base from", "the way to make it comparable is named")

	// Equal digests are equal whatever composed the base.
	same := BaseIdentity{Composed: "0.6.7", Running: "0.6.8", SourceRef: relName + "@" + relDig, BaseDigest: baseD, ReleaseRef: relName + "@" + relDig}
	assert.Equal(t, StatusOK, BaseReleaseCheck{Inspect: func() (BaseIdentity, error) { return same, nil }}.Run(context.Background()).Status)

	// A base that does not say what composed it cannot be compared either.
	unknown := BaseIdentity{Running: "0.6.8", SourceRef: relName + "@" + movedD, BaseDigest: baseD, ReleaseRef: relName + "@" + relDig}
	assert.Equal(t, StatusNotChecked, BaseReleaseCheck{Inspect: func() (BaseIdentity, error) { return unknown, nil }}.Run(context.Background()).Status)
}
