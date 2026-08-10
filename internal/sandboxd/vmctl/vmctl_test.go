package vmctl

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHandler records what the child was asked to do.
type fakeHandler struct {
	mu      sync.Mutex
	entries []string
	drain   bool
	calls   int
	err     error
	resp    Response
}

func (f *fakeHandler) SetPolicy(entries []string, drain bool) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.entries, f.drain = entries, drain
	return f.resp, f.err
}

// GetPolicy reports what the fake child claims to be enforcing.
func (f *fakeHandler) GetPolicy() (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return Response{}, f.err
	}
	return Response{Entries: f.entries, Rules: len(f.entries)}, nil
}

func (f *fakeHandler) snapshot() ([]string, bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries, f.drain, f.calls
}

// serveOn starts a child-side control server on a short temp socket and
// returns the client bound to it.
func serveOn(t *testing.T, h Handler) (Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "vc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, SocketName)

	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = Serve(ln, h) }()
	return Client{SocketPath: sock}, sock
}

// TestSetPolicy_ReachesTheChildAndReportsWhatItDid is the channel's reason for
// existing: the daemon acts on state that lives in another process.
// Refs: MGIT-74, MGIT-72
func TestSetPolicy_ReachesTheChildAndReportsWhatItDid(t *testing.T) {
	h := &fakeHandler{resp: Response{Killed: 3, Rules: 1}}
	client, _ := serveOn(t, h)

	resp, err := client.SetPolicy([]string{"registry.example:443"}, false)

	require.NoError(t, err)
	assert.True(t, resp.OK)
	assert.Equal(t, 3, resp.Killed, "the child reports what it terminated, not what it was asked to")
	assert.Equal(t, 1, resp.Rules)
	entries, drain, calls := h.snapshot()
	assert.Equal(t, []string{"registry.example:443"}, entries)
	assert.False(t, drain)
	assert.Equal(t, 1, calls)
}

// TestSetPolicy_DrainIsCarriedThrough verifies the opt-in weaker behavior
// crosses the wire rather than being lost to a default. Refs: ADR-011
func TestSetPolicy_DrainIsCarriedThrough(t *testing.T) {
	h := &fakeHandler{resp: Response{Drained: true}}
	client, _ := serveOn(t, h)

	resp, err := client.SetPolicy(nil, true)

	require.NoError(t, err)
	assert.True(t, resp.Drained)
	_, drain, _ := h.snapshot()
	assert.True(t, drain)
}

// TestSetPolicy_UnreachableChannel_FailsClosed is the property that matters
// most: a revoke that cannot reach the VM must REPORT FAILURE. Reporting
// success while the VM keeps enforcing the old policy would have the caller
// run untrusted code believing egress was closed. Refs: MGIT-74, MGIT-72
func TestSetPolicy_UnreachableChannel_FailsClosed(t *testing.T) {
	client := Client{SocketPath: filepath.Join(t.TempDir(), "absent.sock")}

	resp, err := client.SetPolicy(nil, false)

	require.Error(t, err)
	assert.False(t, resp.OK)
	assert.Contains(t, err.Error(), "NOT changed",
		"the error must say the policy did not change, not merely that something failed")
	assert.Contains(t, err.Error(), "unreachable")
}

// TestSetPolicy_ChildRefusal_IsSurfaced verifies a child-side failure reaches
// the caller as an error rather than an OK response.
func TestSetPolicy_ChildRefusal_IsSurfaced(t *testing.T) {
	h := &fakeHandler{err: errors.New("policy does not compile")}
	client, _ := serveOn(t, h)

	resp, err := client.SetPolicy([]string{"bad !!"}, false)

	require.Error(t, err)
	assert.False(t, resp.OK)
	assert.Contains(t, err.Error(), "policy does not compile")
}

// TestServe_UnknownOp_IsRefused verifies an unrecognized verb is refused
// rather than dropped: a silently ignored op would look like success.
func TestServe_UnknownOp_IsRefused(t *testing.T) {
	client, _ := serveOn(t, &fakeHandler{})

	resp, err := client.do(Request{Op: "no-such-op"})

	require.Error(t, err)
	assert.False(t, resp.OK)
	assert.Contains(t, err.Error(), "unknown control op")
}

// TestServe_MalformedRequest_IsRefusedNotCrashed verifies garbage on the
// channel is answered with an error rather than taking the child down — the
// child is holding a running VM.
func TestServe_MalformedRequest_IsRefusedNotCrashed(t *testing.T) {
	h := &fakeHandler{}
	client, sock := serveOn(t, h)

	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	_, _ = conn.Write([]byte("this is not json\n"))
	_ = conn.Close()

	// The server is still serving afterwards.
	_, err = client.SetPolicy(nil, false)
	require.NoError(t, err)
	_, _, calls := h.snapshot()
	assert.Equal(t, 1, calls)
}

// TestServe_ConcurrentRequests verifies the channel handles overlapping
// control calls without corruption — the daemon may serve several callers.
func TestServe_ConcurrentRequests(t *testing.T) {
	h := &fakeHandler{resp: Response{Rules: 1}}
	client, _ := serveOn(t, h)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.SetPolicy([]string{"registry.example:443"}, false)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	_, _, calls := h.snapshot()
	assert.Equal(t, 16, calls)
}

// TestSocketName_FitsTheSunPathBudget guards the constraint this backend has
// already tripped over once: the control socket must stay short enough that a
// realistic state-dir path plus this name fits sun_path. Refs: MGIT-61.15
func TestSocketName_FitsTheSunPathBudget(t *testing.T) {
	assert.LessOrEqual(t, len(SocketName), 8,
		"the control socket name must stay short; every byte comes out of the "+
			"104-byte sun_path budget the whole per-VM path shares")
}

// TestGetPolicy_ReportsWhatTheChildIsEnforcing verifies the host can READ the
// live policy out of the VM child, not only write it.
//
// Without a read, the only observable policy is the launch-time one, which a
// live mutation makes wrong — so a caller could not confirm a revoke took
// effect. Refs: MGIT-72, MGIT-74
func TestGetPolicy_ReportsWhatTheChildIsEnforcing(t *testing.T) {
	h := &fakeHandler{entries: []string{"registry.example:443"}}
	client, _ := serveOn(t, h)

	resp, err := client.GetPolicy()

	require.NoError(t, err)
	assert.True(t, resp.OK)
	assert.Equal(t, []string{"registry.example:443"}, resp.Entries)
	assert.Equal(t, 1, resp.Rules)
}

// TestGetPolicy_UnreachableChild_FailsClosed verifies a missing child is an
// actionable error, never an empty policy — "nothing is allowed" and "nothing
// is enforcing" must not look the same. Refs: MGIT-72, SEC-04
func TestGetPolicy_UnreachableChild_FailsClosed(t *testing.T) {
	client := Client{SocketPath: filepath.Join(t.TempDir(), "absent.sock")}

	_, err := client.GetPolicy()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreachable")
}

// TestDispatch_UnknownOp_IsRefused verifies a verb the child does not know is
// REPORTED, never silently dropped: a dropped verb looks like success to the
// host, which for a revoke means believing egress is closed when it is open.
// Refs: MGIT-72, MGIT-74
func TestDispatch_UnknownOp_IsRefused(t *testing.T) {
	resp := dispatch(Request{Op: "widen-everything"}, &fakeHandler{})

	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "unknown control op")
}
