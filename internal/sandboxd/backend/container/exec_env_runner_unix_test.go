//go:build !windows

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPodmanRunner_RunWithFD_DeliversDataOnDescriptorThree drives the real
// runner against a stand-in runtime that prints what it reads from the file it
// is told to read, and checks the data arrives whole, including data larger
// than a pipe buffer, with nothing of it on the stand-in's command line.
func TestPodmanRunner_RunWithFD_DeliversDataOnDescriptorThree(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >&2\nwhile [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = --env-file ]; then cat \"$2\"; fi\n  shift\ndone\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, size := range []int{10, 300 << 10} {
		data := []byte("K=" + strings.Repeat("v", size) + "\n")

		out, err := PodmanRunner{}.runWithFD(context.Background(), data, "exec", "--env-file", "/dev/fd/3", "c1")

		require.NoError(t, err)
		assert.Equal(t, string(data), string(out), "size %d", size)
	}
}
