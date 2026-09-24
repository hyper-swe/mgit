package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

func fixtureDigest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// LIKE WITH LIKE (MGIT-223). Since 0.6.8 a compose records the image INDEX
// a tag resolves to, where an older compose recorded the PLATFORM MANIFEST
// the index selected for its host. Recomposing an older base printed "NOTE:
// <tag> now resolves to a different image" every time, although the index
// selected the very platform manifest the old record names: the same bytes.
// The record now keeps the platform manifest it selected beside the index,
// and a recompose compares each kind with its own kind. The records here are
// fixtures written through the real journal and lock, as a compose writes
// them, with the older record shaped as 0.6.7 wrote it. Refs: MGIT-223,
// MGIT-219, MGIT-147, MGIT-218
func TestRecompose_ComparesLikeWithLike(t *testing.T) {
	p, q := fixtureDigest("a"), fixtureDigest("b")
	i1, i2 := fixtureDigest("c"), fixtureDigest("d")
	ref := func(digest, platform string) guestbase.Ref {
		return guestbase.Ref{Registry: "registry-1.docker.io", Repository: "library/debian", Tag: "12",
			Digest: digest, SelectedPlatform: platform}
	}
	tests := []struct {
		name          string
		first, second guestbase.Ref
		want, not     []string
		moved         bool
	}{
		{"an_older_record_of_the_platform_manifest_the_index_still_selects", ref(p, ""), ref(i1, p),
			[]string{"resolves to the same image", "platform manifest", "image index"}, []string{"now resolves to a different image"}, false},
		{"an_older_record_and_a_different_platform_manifest", ref(p, ""), ref(i1, q),
			[]string{"now resolves to a different image", p, q}, nil, true},
		{"the_index_moved_and_this_hosts_manifest_did_not", ref(i1, p), ref(i2, p),
			[]string{"image index moved", "unchanged"}, []string{"now resolves to a different image"}, false},
		{"nothing_moved", ref(i1, p), ref(i1, p), nil, []string{"NOTE", "now resolves to a different image", "same image"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hostRoot := t.TempDir()
			priv, err := images.EnsureSigningKey(context.Background(), hostRoot, printTrustRootAuditor{w: io.Discard})
			require.NoError(t, err)
			clock := func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
			opts := composeOptions{name: "base"}
			_, err = registerComposedBase(hostRoot, basecache.Entry{Digest: fixtureDigest("1")}, tt.first, opts, signWith(priv), clock)
			require.NoError(t, err)
			res, err := registerComposedBase(hostRoot, basecache.Entry{Digest: fixtureDigest("2")}, tt.second, opts, signWith(priv), clock)
			require.NoError(t, err)

			var out bytes.Buffer
			reportComposition(&out, res)
			for _, w := range tt.want {
				assert.Contains(t, out.String(), w)
			}
			for _, n := range tt.not {
				assert.NotContains(t, out.String(), n)
			}
			assert.Equal(t, tt.moved, res.Record.TagMoved(), "the journal says a tag moved only when the image did")
			assert.Equal(t, tt.moved, composeJSON(res)["tag_moved"] == true, "and so does the JSON")
		})
	}
}
