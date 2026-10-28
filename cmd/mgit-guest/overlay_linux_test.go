//go:build linux

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mount-point repair mounts an overlay AT the directory, with the
// directory as its own lower layer and an upper and work directory in memory;
// nothing is copied. Refs: MGIT-230.8

type overlayRecorder struct {
	made    []string
	mounts  []string // fstype source=target data
	copied  []string
	mkErr   error
	mntErr  error
	copyErr error
}

func (r *overlayRecorder) ops(log *slog.Logger) overlayOps {
	return overlayOps{
		mkdirAll: func(p string, _ os.FileMode) error { r.made = append(r.made, p); return r.mkErr },
		mount: func(src, target, fstype string, _ uintptr, data string) error {
			r.mounts = append(r.mounts, fstype+" "+src+"="+target+" "+data)
			return r.mntErr
		},
		copyShadow: func(d string) error { r.copied = append(r.copied, d); return r.copyErr },
		logger:     log,
	}
}

func TestOverlayShadow_MountsAnOverlayAtTheDirectoryWithItselfAsTheLowerLayer(t *testing.T) {
	var r overlayRecorder
	require.NoError(t, overlayShadow("/home", r.ops(nil)))

	require.Len(t, r.mounts, 1)
	m := r.mounts[0]
	assert.True(t, strings.HasPrefix(m, "overlay overlay=/home "), m)
	assert.Contains(t, m, "lowerdir=/home,")
	assert.Contains(t, m, "upperdir=/tmp/.mgit-mp-home/upper,")
	assert.Contains(t, m, "workdir=/tmp/.mgit-mp-home/work")
	assert.Equal(t, []string{"/tmp/.mgit-mp-home/upper", "/tmp/.mgit-mp-home/work"}, r.made)
	assert.Empty(t, r.copied, "a mounted overlay copies nothing")
}

func TestOverlayShadow_ANestedDirectoryGetsItsOwnScratch(t *testing.T) {
	var r overlayRecorder
	require.NoError(t, overlayShadow("/var/lib/data", r.ops(nil)))
	assert.Contains(t, r.mounts[0], "upperdir=/tmp/.mgit-mp-var_lib_data/upper,")
}

func TestOverlayShadow_AnOverlayRefusalFallsBackToTheSeededCopyAndSaysSo(t *testing.T) {
	var buf bytes.Buffer
	r := overlayRecorder{mntErr: errors.New("overlay: invalid argument")}
	require.NoError(t, overlayShadow("/home", r.ops(slog.New(slog.NewTextHandler(&buf, nil)))))

	assert.Equal(t, []string{"/home"}, r.copied, "the repair that worked before still runs")
	assert.Contains(t, buf.String(), "event=mount_point_overlay_refused")
	assert.Contains(t, buf.String(), "overlay: invalid argument")
}

func TestOverlayShadow_AScratchDirectoryRefusalAlsoFallsBack(t *testing.T) {
	r := overlayRecorder{mkErr: errors.New("no space")}
	require.NoError(t, overlayShadow("/home", r.ops(nil)))
	assert.Empty(t, r.mounts, "no mount is attempted without its scratch directories")
	assert.Equal(t, []string{"/home"}, r.copied)
}

func TestOverlayShadow_AFailedFallbackIsTheError(t *testing.T) {
	r := overlayRecorder{mntErr: errors.New("refused"), copyErr: errors.New("copy failed")}
	err := overlayShadow("/home", r.ops(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "copy failed")
}
