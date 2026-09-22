package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/doctor"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
)

// TWO LANES, TWO IMAGES, ONE VERDICT — until the row carries the identity.
//
// Each lane recomposes "under this substrate"; the images differ (a moving
// tag gives each recompose whatever it points at that minute). doctor's
// base/currency read `ok … composed by this substrate` on both, because the
// row answered which SUBSTRATE composed the base and never which IMAGE. The
// inspection now returns the base's identity from images.lock — the resolved
// source and the composed digest — and the two rows read apart.
// Refs: MGIT-218, MGIT-174, MGIT-147
func TestInspectBaseCurrency_TwoLanesComposedFromDifferentImages_AreToldApart(t *testing.T) {
	compose := func(files map[string]string) doctor.BaseIdentity {
		srv, ref := fakeImageServer(t, files)
		defer srv.Close()
		repo := newRepo(t)
		_, err := initTrustRoot(t, repo)
		require.NoError(t, err)
		out, err := runBase(t, repo, "from", ref, "--guest-bin-dir", fakeGuestBins(t), "--plain-http")
		require.NoError(t, err, "base from: %s", out)
		t.Chdir(repo)
		id, err := inspectBaseCurrency()
		require.NoError(t, err)
		return id
	}
	laneA := compose(map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian\nVERSION=12.6"})
	laneB := compose(map[string]string{"bin/sh": "#!/bin/sh", "etc/os-release": "ID=debian\nVERSION=12.7"})

	for _, id := range []doctor.BaseIdentity{laneA, laneB} {
		assert.Equal(t, Version, id.Composed, "both lanes recomposed under this substrate")
		assert.Equal(t, Version, id.Running)
		assert.NotEmpty(t, id.BaseDigest, "the composed tree's digest, as pinned in images.lock")
		assert.NotEmpty(t, guestbase.SourceDigest(id.SourceRef), "the resolved source carries its digest: %q", id.SourceRef)
	}
	assert.NotEqual(t, laneA.SourceRef, laneB.SourceRef, "two images, two sources")
	assert.NotEqual(t, laneA.BaseDigest, laneB.BaseDigest, "two images, two bases")

	rowA := doctor.BaseCurrencyCheck{Inspect: func() (doctor.BaseIdentity, error) { return laneA, nil }}.Run(context.Background())
	rowB := doctor.BaseCurrencyCheck{Inspect: func() (doctor.BaseIdentity, error) { return laneB, nil }}.Run(context.Background())
	assert.Equal(t, doctor.StatusOK, rowA.Status)
	assert.Equal(t, doctor.StatusOK, rowB.Status)
	assert.NotEqual(t, rowA.Summary, rowB.Summary, "the doctor rows of two lanes on different bases must read apart")
	assert.Contains(t, rowA.Summary, laneA.BaseDigest)
	assert.Contains(t, rowB.Summary, laneB.BaseDigest)
}
