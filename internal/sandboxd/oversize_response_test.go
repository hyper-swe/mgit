package sandboxd

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// A response that cannot be SENT must still be a response.
//
// WriteResponse refuses anything over MaxResponseBytes and writes NOTHING;
// writeResponse logged that daemon-side and returned; the deferred close then
// gave the client a bare "read response: EOF". Nothing on the wire, the only
// record of the cause inside the daemon, and a client error naming neither the
// cause nor a next step.
//
// That is the shape of the founder's leg-4 failure: a sync classification over
// a worktree with a host-side node_modules enumerates far more paths than 1 MiB
// of JSON holds, so the answer was never sent. A crash where a refusal
// belonged. Refs: MGIT-160
func TestWriteResponse_TooLargeToSend_IsRefusedLegiblyNotDropped(t *testing.T) {
	// A report far over the cap, as a real classification of a node_modules is.
	huge := make([]string, 40_000)
	for i := range huge {
		huge[i] = "node_modules/@scope/package-" + strings.Repeat("x", 40) + "/dist/index.js"
	}
	resp := &controlproto.Response{Synced: &model.WorktreeSyncReport{Updated: huge}}

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	d := &Daemon{cfg: testDaemonCfg(t)}
	go func() {
		d.writeResponse(server, resp)
		_ = server.Close()
	}()

	got, err := controlproto.ReadResponse(client)
	require.NoError(t, err, "the client must receive a response, not EOF")
	require.NotNil(t, got)
	require.NotEmpty(t, got.Error, "an unsendable response must arrive as an error")

	assert.Contains(t, got.Error, "too large",
		"the refusal must say what went wrong in the caller's terms")
	assert.Contains(t, got.Error, "1048576", "it must name the cap it hit")
	assert.NotContains(t, strings.ToLower(got.Error), "eof")
}

// The replacement must itself fit, whatever the original held: a refusal built
// from the thing that was too big would fail the same way. Refs: MGIT-160
func TestWriteResponse_TheRefusalIsAlwaysSmallEnoughToSend(t *testing.T) {
	huge := make([]string, 60_000)
	for i := range huge {
		huge[i] = strings.Repeat("p", 200)
	}
	resp := &controlproto.Response{
		Synced: &model.WorktreeSyncReport{Updated: huge, Deleted: huge, Detail: strings.Repeat("d", 1<<20)},
	}

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	d := &Daemon{cfg: testDaemonCfg(t)}
	go func() {
		d.writeResponse(server, resp)
		_ = server.Close()
	}()

	got, err := controlproto.ReadResponse(client)
	require.NoError(t, err)
	require.NotEmpty(t, got.Error)
	assert.Nil(t, got.Synced, "the refusal must not carry the payload that could not be sent")
}

// An ordinary response is untouched. Refs: MGIT-160
func TestWriteResponse_NormalResponse_IsUnchanged(t *testing.T) {
	resp := &controlproto.Response{Synced: &model.WorktreeSyncReport{Updated: []string{"a.txt"}, DryRun: true}}

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	d := &Daemon{cfg: testDaemonCfg(t)}
	go func() {
		d.writeResponse(server, resp)
		_ = server.Close()
	}()

	got, err := controlproto.ReadResponse(client)
	require.NoError(t, err)
	assert.Empty(t, got.Error)
	require.NotNil(t, got.Synced)
	assert.Equal(t, []string{"a.txt"}, got.Synced.Updated)
	assert.True(t, got.Synced.DryRun)
}

// testDaemonCfg is the minimum config writeResponse needs.
func testDaemonCfg(t *testing.T) Config {
	t.Helper()
	cfg, _ := testConfig(t, newFakeManager())
	return cfg
}

var _ = context.Background
