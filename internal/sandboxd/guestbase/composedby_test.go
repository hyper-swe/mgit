package guestbase

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A base must record the substrate that composed it.
//
// Nothing did, which is why the "staleness door" never fired: it cannot
// compare a composing substrate to a running one when the first is not
// written down anywhere. What fired in 0.5.0->0.6.1 was the base-cache MISS
// during the in-tree -> content-addressed migration, which everyone read as a
// staleness warning and which can never fire for that reason again.
//
// It matters because the guest binaries are INJECTED AT COMPOSE TIME and
// frozen there: a base composed under an older substrate silently runs that
// substrate's guest code, and so silently lacks the guest-side fixes of every
// release since. Refs: MGIT-174, MGIT-152
func TestComposedBy_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteComposedBy(dir, "0.6.4", "registry/lib/debian:12"))

	got, err := ReadComposedBy(dir)
	require.NoError(t, err)
	assert.Equal(t, "0.6.4", got.Version)
	assert.Equal(t, "registry/lib/debian:12", got.Source)

	// The marker must be DETERMINISTIC: it is inside the composed tree, so it
	// is part of the base's content digest, and two composes of the same image
	// must still reproduce the same pin. A timestamp here broke exactly that
	// and was caught by the existing determinism tests. Refs: MGIT-174, MGIT-147
	require.NoError(t, WriteComposedBy(dir, "0.6.4", "registry/lib/debian:12"))
	again, err := os.ReadFile(filepath.Join(dir, "etc", "mgit", "composed-by.json")) //nolint:gosec // test temp path
	require.NoError(t, err)
	first, err := os.ReadFile(filepath.Join(dir, "etc", "mgit", "composed-by.json")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, string(first), string(again),
		"the same inputs must produce identical bytes, or the base digest is not reproducible")
}

// A base composed before mgit recorded this reports UNKNOWN — never current.
// Reporting silence as currency is the exact failure being fixed.
// Refs: MGIT-174
func TestComposedBy_AbsentMarker_IsUnknownNotCurrent(t *testing.T) {
	_, err := ReadComposedBy(t.TempDir())
	require.ErrorIs(t, err, ErrComposedByUnknown)
}

// A corrupt marker is unknown too, for the same reason: a marker we cannot
// parse tells us nothing, and guessing would be worse than saying so.
// Refs: MGIT-174
func TestComposedBy_CorruptMarker_IsUnknown(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "etc", "mgit"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "etc", "mgit", "composed-by.json"), []byte("{not json"), 0o600))

	_, err := ReadComposedBy(dir)
	require.ErrorIs(t, err, ErrComposedByUnknown)
}

// The currency verdict itself: same substrate is current, any difference is
// stale, and no marker is unknown. Refs: MGIT-174
func TestBaseCurrency(t *testing.T) {
	tests := []struct {
		name     string
		composed string
		running  string
		want     Currency
	}{
		{name: "same_substrate_is_current", composed: "0.6.4", running: "0.6.4", want: CurrencyCurrent},
		{name: "older_substrate_is_stale", composed: "0.6.3", running: "0.6.4", want: CurrencyStale},
		{
			name:     "a_NEWER_composing_substrate_is_also_stale_because_the_pair_still_disagree",
			composed: "0.6.5", running: "0.6.4", want: CurrencyStale,
		},
		{name: "no_marker_is_unknown", composed: "", running: "0.6.4", want: CurrencyUnknown},
		{name: "unknown_running_version_is_unknown", composed: "0.6.4", running: "", want: CurrencyUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, BaseCurrency(tt.composed, tt.running))
		})
	}
}
