package sandboxd

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/landwire"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
)

// framedBlob returns a one-frame land pool carrying a blob — enough to
// exercise the channel's decode without building real commits.
func framedBlob(content string) []byte {
	var buf bytes.Buffer
	_ = landwire.WriteFrame(&buf, landwire.ObjBlob, []byte(content))
	return buf.Bytes()
}

func defaultLandLimits() land.Limits { return land.DefaultLimits() }

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

func newTestChannel(dialer LandDialer, limits land.Limits) (*LandChannel, *PeerBinder) {
	binder, _ := binderWithLog()
	return NewLandChannel(binder, dialer, limits, nil), binder
}

// TestLandChannel_PullThenReplay verifies the channel pulls + decodes the pool
// once and replays the exact bytes to the orchestrator's OpenLandStream.
func TestLandChannel_PullThenReplay(t *testing.T) {
	raw := framedBlob("hello")
	dialer := &fakeLandDialer{data: raw}
	c, binder := newTestChannel(dialer, defaultLandLimits())
	binder.Bind("sbx-1", "sbx-1")

	objs, err := c.Pull(context.Background(), "sbx-1")
	require.NoError(t, err)
	assert.Equal(t, 1, dialer.dials, "the guest land channel is dialed once")
	require.Len(t, objs, 1, "the decoded pool is returned for derivation")
	assert.Equal(t, "hello", string(objs[0].Data))

	rc, err := c.OpenLandStream(context.Background(), "sbx-1")
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, raw, got, "the orchestrator replays the exact pulled bytes")

	// One pull serves one land: a second open without a fresh Pull fails.
	_, err = c.OpenLandStream(context.Background(), "sbx-1")
	assert.Error(t, err)
}

// TestLandChannel_UnboundSandbox_FailsClosed verifies an unbound or torn-down
// sandbox is refused before any dial (SEC-10). Refs: SEC-10
func TestLandChannel_UnboundSandbox_FailsClosed(t *testing.T) {
	dialer := &fakeLandDialer{data: framedBlob("x")}
	c, _ := newTestChannel(dialer, defaultLandLimits()) // no binding for sbx-gone

	_, err := c.Pull(context.Background(), "sbx-gone")
	assert.ErrorIs(t, err, model.ErrPeerBindingMismatch)
	assert.Zero(t, dialer.dials, "an unbound sandbox is never dialed")
}

// TestLandChannel_TornDown_FailsClosed verifies that after teardown the land
// pull is refused even if a buffer was never pulled. Refs: SEC-10, FR-17.27
func TestLandChannel_TornDown_FailsClosed(t *testing.T) {
	dialer := &fakeLandDialer{data: framedBlob("x")}
	c, binder := newTestChannel(dialer, defaultLandLimits())
	binder.Bind("sbx-1", "sbx-1")
	binder.Invalidate("sbx-1")
	_, err := c.Pull(context.Background(), "sbx-1")
	assert.ErrorIs(t, err, model.ErrPeerBindingMismatch)
}

// TestLandChannel_OverBudgetPool_Refused verifies a pool larger than the host
// budget is refused rather than buffered. Refs: FR-17.35
func TestLandChannel_OverBudgetPool_Refused(t *testing.T) {
	dialer := &fakeLandDialer{data: make([]byte, 100)}
	limits := land.Limits{MaxObjectBytes: 64, MaxObjectsPerLand: 10, MaxTotalBytes: 16}
	c, binder := newTestChannel(dialer, limits)
	binder.Bind("sbx-1", "sbx-1")
	_, err := c.Pull(context.Background(), "sbx-1")
	assert.Error(t, err)
	// Nothing buffered for a refused pull.
	_, err = c.OpenLandStream(context.Background(), "sbx-1")
	assert.Error(t, err)
}

// TestLandChannel_MalformedPool_Refused verifies an undecodable pool is
// refused and nothing is buffered.
func TestLandChannel_MalformedPool_Refused(t *testing.T) {
	dialer := &fakeLandDialer{data: []byte{0x99, 0, 0, 0, 0}} // unknown object type
	c, binder := newTestChannel(dialer, defaultLandLimits())
	binder.Bind("sbx-1", "sbx-1")
	_, err := c.Pull(context.Background(), "sbx-1")
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
	_, err = c.OpenLandStream(context.Background(), "sbx-1")
	assert.Error(t, err, "a refused pull buffers nothing")
}

func TestLandChannel_DialError_Surfaces(t *testing.T) {
	dialer := &fakeLandDialer{dialErr: errAssertDial}
	c, binder := newTestChannel(dialer, defaultLandLimits())
	binder.Bind("sbx-1", "sbx-1")
	_, err := c.Pull(context.Background(), "sbx-1")
	assert.Error(t, err)
}

func TestLandChannel_Discard_DropsBuffer(t *testing.T) {
	dialer := &fakeLandDialer{data: framedBlob("x")}
	c, binder := newTestChannel(dialer, defaultLandLimits())
	binder.Bind("sbx-1", "sbx-1")
	_, err := c.Pull(context.Background(), "sbx-1")
	require.NoError(t, err)
	c.Discard("sbx-1")
	_, err = c.OpenLandStream(context.Background(), "sbx-1")
	assert.Error(t, err, "a discarded pull leaves no buffer")
}

var errAssertDial = io.ErrUnexpectedEOF
