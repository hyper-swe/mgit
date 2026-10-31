package microvm

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// floodGuest answers the readiness probe by streaming frames until the host
// stops reading, counting the payload bytes the host actually took. net.Pipe
// is synchronous, so the count is what the host read, which is what it could
// have held in memory.
type floodGuest struct {
	kind   byte
	chunk  []byte
	taken  atomic.Int64
	frames atomic.Int64
}

func (g *floodGuest) DialGuest(ctx context.Context, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		if _, err := execwire.ReadRequest(server); err != nil {
			return
		}
		for ctx.Err() == nil {
			if err := execwire.WriteFrame(server, g.kind, g.chunk); err != nil {
				return
			}
			g.taken.Add(int64(len(g.chunk)))
			g.frames.Add(1)
		}
	}()
	return client, nil
}

// A guest that answers the readiness probe with a flood of output is refused
// by name once the probe's cap is passed, and the host has read no more than
// that cap plus the frame in flight: the output is never accumulated.
// Refs: HSWE-17.7.2
func TestLaunch_ProbeOutputFlood_IsRefusedAtTheCapAndNeverBuffered(t *testing.T) {
	const chunk = 16 << 10
	for name, kind := range map[string]byte{"stdout": execwire.FrameStdout, "stderr": execwire.FrameStderr} {
		t.Run(name, func(t *testing.T) {
			g := &floodGuest{kind: kind, chunk: make([]byte, chunk)}
			mgr := execManager(t, g)
			start := time.Now()
			_, err := mgr.Launch(context.Background(), launchOpts("HSWE-17.7.2", model.NetworkModeNone))
			require.ErrorIs(t, err, model.ErrGuestProbeOutput)
			assert.Less(t, time.Since(start), 250*time.Millisecond, "refused at the cap, not at the deadline")
			assert.LessOrEqual(t, g.taken.Load(), int64(probeOutputCapBytes+chunk),
				"the host read no more than the cap plus the frame in flight")
			assert.Contains(t, err.Error(), "64 KiB", "the message states the limit")
		})
	}
}

// Endless empty frames carry no bytes, so a byte cap alone never fires; the
// frame count is capped too.
func TestLaunch_ProbeEmptyFrameFlood_IsRefused(t *testing.T) {
	g := &floodGuest{kind: execwire.FrameStdout, chunk: nil}
	mgr := execManager(t, g)
	_, err := mgr.Launch(context.Background(), launchOpts("HSWE-17.7.2", model.NetworkModeNone))
	require.ErrorIs(t, err, model.ErrGuestProbeOutput)
	assert.LessOrEqual(t, g.frames.Load(), int64(probeFrameCap+1))
}
