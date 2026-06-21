package sandboxd

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// memConn is a net.Conn whose reads drain a fixed buffer and whose writes
// are discarded — enough to stand in for a dialed guest land channel.
type memConn struct {
	r io.Reader
}

func (c *memConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *memConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *memConn) Close() error                     { return nil }
func (c *memConn) LocalAddr() net.Addr              { return nil }
func (c *memConn) RemoteAddr() net.Addr             { return nil }
func (c *memConn) SetDeadline(time.Time) error      { return nil }
func (c *memConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(time.Time) error { return nil }

// fakeLandDialer returns a memConn over fixed bytes, or a dial error.
type fakeLandDialer struct {
	data    []byte
	dialErr error
	dials   int
}

func (d *fakeLandDialer) DialGuest(_ context.Context, _ string) (net.Conn, error) {
	d.dials++
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	return &memConn{r: newByteReader(d.data)}, nil
}

func newByteReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func newTestChannel(dialer LandDialer, maxBytes int64) (*LandChannel, *PeerBinder) {
	binder, _ := binderWithLog()
	return NewLandChannel(binder, dialer, maxBytes, nil), binder
}

// TestLandChannel_PullThenReplay verifies the channel pulls the pool once and
// replays the exact bytes to the orchestrator's OpenLandStream.
func TestLandChannel_PullThenReplay(t *testing.T) {
	dialer := &fakeLandDialer{data: []byte("framed-pool-bytes")}
	c, binder := newTestChannel(dialer, 1<<20)
	binder.Bind("sbx-1", "sbx-1")

	require.NoError(t, c.Pull(context.Background(), "sbx-1"))
	assert.Equal(t, 1, dialer.dials, "the guest land channel is dialed once")

	rc, err := c.OpenLandStream(context.Background(), "sbx-1")
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "framed-pool-bytes", string(got), "the orchestrator replays the exact pulled bytes")

	// One pull serves one land: a second open without a fresh Pull fails.
	_, err = c.OpenLandStream(context.Background(), "sbx-1")
	assert.Error(t, err)
}

// TestLandChannel_UnboundSandbox_FailsClosed verifies an unbound or torn-down
// sandbox is refused before any dial (SEC-10). Refs: SEC-10
func TestLandChannel_UnboundSandbox_FailsClosed(t *testing.T) {
	dialer := &fakeLandDialer{data: []byte("x")}
	c, _ := newTestChannel(dialer, 1<<20) // no binding for sbx-gone

	err := c.Pull(context.Background(), "sbx-gone")
	assert.ErrorIs(t, err, model.ErrPeerBindingMismatch)
	assert.Zero(t, dialer.dials, "an unbound sandbox is never dialed")
}

// TestLandChannel_TornDown_FailsClosed verifies that after teardown the land
// pull is refused even if a buffer was never pulled. Refs: SEC-10, FR-17.27
func TestLandChannel_TornDown_FailsClosed(t *testing.T) {
	dialer := &fakeLandDialer{data: []byte("x")}
	c, binder := newTestChannel(dialer, 1<<20)
	binder.Bind("sbx-1", "sbx-1")
	binder.Invalidate("sbx-1")
	assert.ErrorIs(t, c.Pull(context.Background(), "sbx-1"), model.ErrPeerBindingMismatch)
}

// TestLandChannel_OverBudgetPool_Refused verifies a pool larger than the host
// budget is refused rather than buffered. Refs: FR-17.35
func TestLandChannel_OverBudgetPool_Refused(t *testing.T) {
	dialer := &fakeLandDialer{data: make([]byte, 100)}
	c, binder := newTestChannel(dialer, 16) // 16-byte budget
	binder.Bind("sbx-1", "sbx-1")
	assert.Error(t, c.Pull(context.Background(), "sbx-1"))
	// Nothing buffered for a refused pull.
	_, err := c.OpenLandStream(context.Background(), "sbx-1")
	assert.Error(t, err)
}

func TestLandChannel_DialError_Surfaces(t *testing.T) {
	dialer := &fakeLandDialer{dialErr: assertDialErr}
	c, binder := newTestChannel(dialer, 1<<20)
	binder.Bind("sbx-1", "sbx-1")
	assert.Error(t, c.Pull(context.Background(), "sbx-1"))
}

func TestLandChannel_Discard_DropsBuffer(t *testing.T) {
	dialer := &fakeLandDialer{data: []byte("x")}
	c, binder := newTestChannel(dialer, 1<<20)
	binder.Bind("sbx-1", "sbx-1")
	require.NoError(t, c.Pull(context.Background(), "sbx-1"))
	c.Discard("sbx-1")
	_, err := c.OpenLandStream(context.Background(), "sbx-1")
	assert.Error(t, err, "a discarded pull leaves no buffer")
}

var assertDialErr = io.ErrUnexpectedEOF
