package guestbase

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frozen is an injected clock; the journal must not reach for the wall clock.
func frozen(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestRecordCompose_AppendsRatherThanReplacing(t *testing.T) {
	hostRoot := t.TempDir()
	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

	first := Compose{
		Name:       "base",
		SourceTag:  "registry-1.docker.io/library/golang:1.26-bookworm",
		SourceRef:  "registry-1.docker.io/library/golang:1.26-bookworm@sha256:5290458b",
		BaseDigest: "sha256:aaa",
	}
	require.NoError(t, RecordCompose(hostRoot, first, frozen(at)))

	second := Compose{
		Name:           "base",
		SourceTag:      "registry-1.docker.io/library/golang:1.26-bookworm",
		SourceRef:      "registry-1.docker.io/library/golang:1.26-bookworm@sha256:8ed5e7e2",
		BaseDigest:     "sha256:bbb",
		PrevSourceRef:  first.SourceRef,
		PrevBaseDigest: first.BaseDigest,
	}
	require.NoError(t, RecordCompose(hostRoot, second, frozen(at.Add(24*time.Hour))))

	history, err := ComposeHistory(hostRoot)
	require.NoError(t, err)
	require.Len(t, history, 2, "the journal must keep what it superseded")
	assert.Equal(t, "sha256:aaa", history[0].BaseDigest, "the earlier record was rewritten")
	assert.Equal(t, "sha256:bbb", history[1].BaseDigest)
	assert.Equal(t, "2026-08-20T09:00:00Z", history[0].RecordedAt)
	assert.Equal(t, "sha256:5290458b", SourceDigest(history[1].PrevSourceRef),
		"a superseding record must name what it superseded")
}

func TestComposeHistory_WithNoJournal_IsEmptyNotAnError(t *testing.T) {
	history, err := ComposeHistory(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, history)
}

func TestComposeHistory_WithACorruptJournal_ReportsIt(t *testing.T) {
	hostRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hostRoot, ProvenanceFileName), []byte("{not json\n"), 0o600))
	_, err := ComposeHistory(hostRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ProvenanceFileName)
}

func TestRecordCompose_WithNoClock_IsRefused(t *testing.T) {
	err := RecordCompose(t.TempDir(), Compose{Name: "base"}, nil)
	require.Error(t, err, "the journal must take an injected clock, never the wall clock")
}

// A tag is a name that can point twice. Telling the three cases apart is the
// whole point of recording tag and digest separately. Refs: MGIT-147
func TestTagMoved_DistinguishesAMovedTagFromADifferentImage(t *testing.T) {
	const repo = "registry-1.docker.io/library/golang"
	tests := []struct {
		name string
		rec  Compose
		want bool
	}{
		{
			name: "same_tag_new_digest_is_a_moved_tag",
			rec: Compose{
				SourceRef:     repo + ":1.26-bookworm@sha256:8ed5e7e2",
				PrevSourceRef: repo + ":1.26-bookworm@sha256:5290458b",
			},
			want: true,
		},
		{
			name: "same_tag_same_digest_did_not_move",
			rec: Compose{
				SourceRef:     repo + ":1.26-bookworm@sha256:5290458b",
				PrevSourceRef: repo + ":1.26-bookworm@sha256:5290458b",
			},
			want: false,
		},
		{
			name: "a_different_tag_is_a_choice_not_a_drift",
			rec: Compose{
				SourceRef:     repo + ":1.27-bookworm@sha256:8ed5e7e2",
				PrevSourceRef: repo + ":1.26-bookworm@sha256:5290458b",
			},
			want: false,
		},
		{
			name: "a_first_compose_supersedes_nothing",
			rec:  Compose{SourceRef: repo + ":1.26-bookworm@sha256:5290458b"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.rec.TagMoved())
		})
	}
}

func TestSourceTagAndDigest_SplitAResolvedReference(t *testing.T) {
	const ref = "registry-1.docker.io/library/debian:12@sha256:" + "abc"
	assert.Equal(t, "registry-1.docker.io/library/debian:12", SourceTag(ref))
	assert.Equal(t, "sha256:abc", SourceDigest(ref))
	assert.Equal(t, "debian:12", SourceTag("debian:12"), "an unresolved reference is all tag")
	assert.Empty(t, SourceDigest("debian:12"))
}

// A pulled reference keeps BOTH halves: the digest makes it repeatable, the
// tag makes it recognizable. Refs: MGIT-147
func TestRefString_KeepsTheTagBesideTheDigest(t *testing.T) {
	ref := Ref{Registry: "registry-1.docker.io", Repository: "library/golang", Tag: "1.26-bookworm"}
	ref.Digest = "sha256:" + strings.Repeat("a", 64)
	assert.Equal(t, "registry-1.docker.io/library/golang:1.26-bookworm@sha256:"+strings.Repeat("a", 64), ref.String())
	assert.Equal(t, "registry-1.docker.io/library/golang:1.26-bookworm", SourceTag(ref.String()))
}

func TestRecordCompose_WithAnUnusableHostRoot_ReportsIt(t *testing.T) {
	// A host root that is a FILE cannot hold a journal; the failure must name
	// the journal rather than surfacing a bare mkdir error.
	notADir := filepath.Join(t.TempDir(), "sandbox")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o600))

	err := RecordCompose(notADir, Compose{Name: "base"}, frozen(time.Now()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "guest base provenance")
}

func TestRecordCompose_WithNoHostRoot_IsRefused(t *testing.T) {
	err := RecordCompose("", Compose{Name: "base"}, frozen(time.Now()))
	require.Error(t, err)
}

func TestComposeHistory_SkipsBlankLines(t *testing.T) {
	hostRoot := t.TempDir()
	require.NoError(t, RecordCompose(hostRoot, Compose{Name: "base", BaseDigest: "sha256:a"}, frozen(time.Now())))
	//nolint:gosec // G306: test-owned journal
	f, err := os.OpenFile(filepath.Join(hostRoot, ProvenanceFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("\n\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	history, err := ComposeHistory(hostRoot)
	require.NoError(t, err)
	assert.Len(t, history, 1)
}
