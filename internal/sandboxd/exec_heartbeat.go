package sandboxd

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// execOutcome is one completed service exec, carried out of the goroutine that
// ran it so the handler goroutine stays free to beat while it runs.
type execOutcome struct {
	res *model.ExecResult
	err error
}

// heartbeatInterval is the cadence this daemon beats at: the shared execwire
// constant, or the Config override tests use to compress it.
func (d *Daemon) heartbeatInterval() time.Duration {
	if d.cfg.HeartbeatInterval > 0 {
		return d.cfg.HeartbeatInterval
	}
	return execwire.HeartbeatInterval
}

// runExec calls the service on its own goroutine and delivers the outcome.
//
// It carries its OWN recover(): handleConn's recover guards the handler
// goroutine, and a panic on a goroutine it merely spawned would take the whole
// daemon down — stranding every running VM unsupervised, which is the exact
// outcome that recover exists to prevent. Refs: MGIT-11.10.8, MGIT-133
func (d *Daemon) runExec(ctx context.Context, args execArgs, done chan<- execOutcome) {
	defer func() {
		if r := recover(); r != nil {
			d.cfg.Logger.Error("sandboxd recovered from exec panic",
				"event", "handler_panic", "panic", fmt.Sprintf("%v", r))
			done <- execOutcome{err: fmt.Errorf("sandbox exec: daemon internal error")}
		}
	}()
	res, err := d.cfg.Service.Exec(ctx, args.taskID, args.req)
	done <- execOutcome{res: res, err: err}
}

// execArgs is the exec's addressing, unpacked from the control request so the
// beat loop and the runner share one small value.
type execArgs struct {
	taskID string
	req    model.ExecRequest
}

// beatWhileExecuting writes a liveness beat every interval until the exec
// completes, and returns its outcome. The bool reports whether the CONNECTION
// survived; false means the client is gone and nothing more should be written.
//
// WHAT A BEAT ASSERTS, precisely, because a liveness signal that overstates
// itself is worse than none: this daemon process is scheduled, this exec's
// handler goroutine is running, this connection is writable, and the sandbox
// service answered for this exec's own sandbox within the last interval. It
// does NOT assert that the guest is making progress — nothing host-side can,
// because the daemon relays a command's output only once it finishes, so
// "backend call still running" and "backend call will never return" are the
// same observable state here. A caller who wants to bound DURATION asks for it
// with ExecRequest.Timeout; a beat bounds SILENCE.
//
// THE BEAT IS GATED ON A PROBE, and that gate is the whole design. A bare
// ticker would keep beating straight through a daemon deadlocked on the
// sandbox registry — certifying a liveness that does not exist, which is
// strictly worse than sending nothing. Every beat here is spent against a
// fresh answer from probeSelf and a stale answer buys at most one, so a daemon
// that stops answering for itself falls silent within two intervals.
// Refs: FR-17.11.1, MGIT-133, MGIT-122
func (d *Daemon) beatWhileExecuting(ctx context.Context, conn net.Conn,
	taskID string, done <-chan execOutcome) (execOutcome, bool) {
	interval := d.heartbeatInterval()
	stop := make(chan struct{})
	defer close(stop)
	answered := d.probeSelf(ctx, taskID, interval, stop)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case out := <-done:
			return out, true
		case <-ticker.C:
		}
		select {
		case <-answered:
			if !d.writeHeartbeat(conn) {
				return execOutcome{}, false
			}
		default:
			// The daemon has not answered for its own state since the last
			// beat. Skip this one: silence is the honest report, and the
			// client's idle deadline turns it into a diagnosis.
		}
	}
}

// probeSelf runs ONE goroutine that repeatedly asks the service for this
// exec's own sandbox and signals every ANSWER on the returned channel.
//
// The answer is the evidence; its content is not — ErrSandboxNotFound is a
// perfectly good proof of life, because what is being tested is whether the
// daemon can still speak about its own state at all. Status takes the sandbox
// service's registry mutex, which is the lock every state transition on this
// daemon passes through, so a daemon deadlocked under the exec path blocks
// here and emits nothing. A frozen process (SIGSTOP, a full deadlock) stops
// this goroutine with all the others.
//
// The channel holds one answer, so an answer can be spent on at most one beat
// and can never accumulate into a burst that outlives the daemon's ability to
// produce it. Refs: MGIT-133
func (d *Daemon) probeSelf(ctx context.Context, taskID string,
	interval time.Duration, stop <-chan struct{}) <-chan struct{} {
	answered := make(chan struct{}, 1)
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
			}
			// A probe that never returns is a daemon that cannot speak for
			// itself; the beat it would have permitted is never written.
			_, _ = d.cfg.Service.Status(ctx, taskID)
			select {
			case answered <- struct{}{}:
			default:
			}
			// Probe faster than the beat so an ordinary tick always has a
			// fresh answer waiting and a healthy daemon never skips a beat.
			timer.Reset(interval / 2)
		}
	}()
	return answered
}

// writeHeartbeat writes one liveness beat under a write deadline and reports
// whether the connection is still usable. A client that hung up mid-exec
// (Ctrl-C) is ordinary, not an incident, so it is logged at debug.
// Refs: MGIT-133
func (d *Daemon) writeHeartbeat(conn net.Conn) bool {
	d.armWriteDeadline(conn)
	if err := execwire.WriteHeartbeat(conn); err != nil {
		d.cfg.Logger.Debug("sandboxd heartbeat write failed",
			"event", "write_error", "error", err.Error())
		return false
	}
	return true
}
