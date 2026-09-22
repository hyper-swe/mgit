package guestbase

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THIS BUILD RECORDS THE BASE IT VOUCHES FOR. A fleet recomposed "from the
// same tag" at different moments sat on different images (MGIT-218); the
// release now records the image and digest it was smoke-tested with, and an
// empty record is a refusal, never a silent "no base". Refs: MGIT-219
func TestReleaseBaseRecord_ThisBuildRecordsAnImageAndADigest(t *testing.T) {
	rec, err := ReleaseBaseRecord()
	require.NoError(t, err, "the embedded release-base.json must name the base this release is smoke-tested with")
	assert.Regexp(t, regexp.MustCompile(`^sha256:[0-9a-f]{64}$`), rec.Digest)
	ref, err := ParseRef(rec.Image)
	require.NoError(t, err)
	assert.NotEmpty(t, ref.Tag, "the image names a tag — documentation for a reader; the digest is the identity")
	assert.Equal(t, rec.Digest, SourceDigest(rec.Ref()), "Ref() carries the digest")
	assert.Equal(t, ref.Registry+"/"+ref.Repository+":"+ref.Tag, SourceTag(rec.Ref()), "Ref() carries the resolved name with the tag")
	parsed, err := ParseRef(rec.Ref())
	require.NoError(t, err)
	assert.Equal(t, rec.Digest, parsed.Digest, "Ref() parses back as a digest reference — what the pull uses")
}

func TestParseReleaseBase_RefusesWhatCannotPin(t *testing.T) {
	const good = "sha256:9a2bafc2cebc397fc253a4b80c6d4bc425eb5a2a819cd25ada003c7facd48f1d"
	tests := []struct {
		name    string
		raw     string
		wantErr error  // sentinel, or nil
		wantMsg string // substring of a non-sentinel error
	}{
		{name: "empty_record_is_the_sentinel", raw: "", wantErr: ErrNoReleaseBase},
		{name: "whitespace_is_empty_too", raw: "\n  \n", wantErr: ErrNoReleaseBase},
		{name: "not_json", raw: "debian:12", wantMsg: "release record"},
		{name: "missing_digest", raw: `{"image":"debian:12"}`, wantMsg: "both are required"},
		{name: "malformed_digest", raw: `{"image":"debian:12","digest":"sha256:short"}`, wantMsg: "release record"},
		{name: "digest_smuggled_into_the_image", raw: `{"image":"debian:12@` + good + `","digest":"` + good + `"}`, wantMsg: "carries a digest"},
		{name: "good", raw: `{"image":"debian:12","digest":"` + good + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := parseReleaseBase([]byte(tt.raw))
			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantMsg != "":
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantMsg)
			default:
				require.NoError(t, err)
				assert.Equal(t, "registry-1.docker.io/library/debian:12@"+good, rec.Ref(), "Hub defaults applied, both halves kept")
			}
		})
	}
}
