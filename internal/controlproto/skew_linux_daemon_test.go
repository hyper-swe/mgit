package controlproto

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The skew remedy lists every install route, go install and a clone's go
// build included. On Linux both of those produce the CGO-free firecracker
// daemon, which boots only a kernel + ext4 rootfs image and refuses sync and
// export, while the release archive carries the libkrun daemon. A Linux user
// on the archive who takes the go line switches backends without being told.
// The release notes and README say so beside the same commands (MGIT-230.10);
// the one message a mixed pair is guaranteed to read now does too.
// Refs: MGIT-230.10.1, MGIT-230.10
func TestSkewMessage_TheGoRoutesSayTheyGiveTheFirecrackerDaemonOnLinux(t *testing.T) {
	msg := SkewMessage(Peer{Protocol: 2, Version: "0.6.0 (commit: aaa)"}, Peer{Protocol: 1, Version: "0.5.0 (commit: bbb)"})
	start, end := strings.Index(msg, "Upgrade both"), strings.Index(msg, "Confirm both")
	require.True(t, start >= 0 && end > start, "the routes block is where the go lines are: %q", msg)
	routes := msg[start:end]
	require.Contains(t, routes, "go install github.com/hyper-swe/mgit/cmd/mgit-sandboxd@latest", "the go route is still offered")
	assert.Contains(t, routes, "firecracker", "and it says what it builds on Linux")
	assert.Contains(t, routes, "libkrun", "and what the release archive carries instead")
}
