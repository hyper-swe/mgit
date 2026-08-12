//go:build linux

// The GUEST-SIDE established-flow precondition for the firecracker live-policy
// e2e (MGIT-96).
//
// WHY THIS EXISTS. The kill/drain assertions in livepolicy_e2e_linux_test.go
// are only meaningful if a flow is ACTUALLY CARRYING DATA when the revoke
// lands. This suite used to establish that from the HOST listener's accept,
// and accepted is not the same as carrying data: if the revoke landed between
// the accept and the guest's first read, the guest reported
// `HOLD = CONNECTED-NO-DATA` and the kill assertion fired on a precondition
// that had not been met — a red gate for a working product.
//
// The libkrun twin (e2e_realvm_livepolicy_test.go) already waits on the
// GUEST's own `PROBE-HOLD ESTABLISHED` line, which the probe prints only after
// real bytes have come back from the destination. This file gives firecracker
// the same wait, in the same shape, so the two backends' proofs remain the same
// observation made twice rather than two that merely sound alike.
// Refs: MGIT-96, MGIT-72, MGIT-92, SEC-04, ADR-012
package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestexec"
)

// fcEstablishedMarker is the probe's "my connection is up and has carried real
// bytes" line. The host waits for it before mutating policy — a fixed sleep, or
// a host-side accept, would race the guest's first read. It is the SAME marker
// the libkrun suite waits for, because it is the same probe.
const fcEstablishedMarker = "PROBE-HOLD ESTABLISHED"

// streamProbe runs one netprobe invocation in the guest and streams its stdout
// through watch AS IT ARRIVES, instead of after the command exits.
//
// It dials the guest's exec channel DIRECTLY rather than going through
// microvm.Manager.Exec, and that is the whole reason it exists: Manager.Exec
// accumulates stdout into a buffer and returns it only once the command has
// finished, so a marker printed mid-command cannot be observed while the
// command is still running — and this suite's precondition is precisely a
// mid-command observation. Dialing the guest is also exactly what the libkrun
// twin's execProbe does, which keeps the two proofs identical in shape.
//
// Bypassing Manager here is safe for this suite and only for it: the fixture's
// control probe has already gone through Manager.Exec, so the guest is proven
// to be serving (the first-command retry has nothing left to do), and
// firecracker delivers the worktree as a launch-time image, so there is no
// host->guest sync for Manager to carry in. Refs: MGIT-96, FR-17.11
func streamProbe(workDir, sandboxID string, watch *markerWatcher, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := newGuestDialer(workDir).DialGuest(ctx, sandboxID)
	if err != nil {
		return "", fmt.Errorf("host could not reach mgit-guest's exec channel: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var stderr bytes.Buffer
	sink := &bytes.Buffer{}
	// Each frame is written as it arrives, so the marker is observed the
	// moment the guest prints it rather than when the probe exits.
	stdout := writerFunc(func(p []byte) (int, error) {
		sink.Write(p)
		watch.observe(string(p))
		return len(p), nil
	})
	if _, err := guestexec.Run(conn, model.ExecRequest{Command: argv}, stdout, &stderr); err != nil {
		return sink.String() + stderr.String(), err
	}
	return sink.String() + stderr.String(), nil
}

// writerFunc adapts a function to io.Writer so the probe's stdout can be both
// accumulated and watched for the establishment marker as it streams.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// markerWatcher fires a channel the first time a marker appears in a stream.
// It matches against the accumulated stream, not the individual chunk, so a
// marker split across two frames is still seen. A nil watcher is a working
// no-op.
type markerWatcher struct {
	marker string
	once   sync.Once
	seen   chan struct{}
	mu     sync.Mutex
	buf    strings.Builder
}

func newMarkerWatcher(marker string) *markerWatcher {
	return &markerWatcher{marker: marker, seen: make(chan struct{})}
}

// observe feeds a chunk of stream output to the watcher.
func (w *markerWatcher) observe(chunk string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.buf.WriteString(chunk)
	hit := strings.Contains(w.buf.String(), w.marker)
	w.mu.Unlock()
	if hit {
		w.once.Do(func() { close(w.seen) })
	}
}

// heldFlow is a netprobe `hold` running in the guest: a connection that stays
// open across a policy mutation, so the mutation has something to terminate.
type heldFlow struct {
	watch *markerWatcher
	done  chan string
	err   error
}

// startHeldFlow launches the netprobe `hold` verb INSIDE the guest — a
// connection that opens, carries real bytes, announces itself and then stays
// open for the window, reporting whether it DIED or SURVIVED. It returns once
// the probe is RUNNING (not once it is established — use awaitEstablished).
//
// It is a real binary rather than a busybox shell pipeline for a reason worth
// keeping on the record: the first version of this helper ran `sleep N | nc ...`
// and timed the pipeline. A shell waits for EVERY member of a pipeline, so the
// elapsed time was N whether the connection was killed at once or never
// touched — an assertion that could not fail in the drain direction and could
// not pass in the kill direction. A probe that reports its own observation has
// no such ambiguity.
//
// It is the SAME probe the libkrun suite uses, so the two backends' proofs are
// stated in identical terms and can be compared directly.
func startHeldFlow(t *testing.T, workDir, sandboxID, probePath, dest string) *heldFlow {
	t.Helper()
	h := &heldFlow{watch: newMarkerWatcher(fcEstablishedMarker), done: make(chan string, 1)}
	go func() {
		out, err := streamProbe(workDir, sandboxID, h.watch,
			probePath, "hold", dest, fmt.Sprintf("%d", fcHoldSeconds))
		h.err = err
		h.done <- out
	}()
	return h
}

// awaitEstablished blocks until the GUEST reports a live connection carrying
// real bytes, and FAILS if it never does — an unestablished flow makes every
// assertion after it vacuous, which is exactly the defect this file closes.
//
// It fails FAST when the probe EXITS before establishing (a denied dial, a
// destination that never replied): that is a better failure than a timeout
// because the probe's own output says what went wrong.
func (h *heldFlow) awaitEstablished(t *testing.T, info *model.SandboxInfo) {
	t.Helper()
	select {
	case <-h.watch.seen:
		t.Log("the guest reports an ESTABLISHED flow carrying real bytes; revoking now")
	case out := <-h.done:
		t.Fatalf("the hold probe exited before it established a connection, so there was "+
			"nothing for the revoke to kill — this is the killed=0 defect; got:\n%s\n%s",
			out, hostNetDiagnostics(t, info))
	case <-time.After(60 * time.Second):
		t.Fatalf("the guest never reported an established connection within 60s, so there "+
			"is nothing for the revoke to kill.\n%s", hostNetDiagnostics(t, info))
	}
}

// await returns the hold probe's full output once it finishes.
func (h *heldFlow) await(t *testing.T) string {
	t.Helper()
	select {
	case out := <-h.done:
		if h.err != nil {
			t.Fatalf("hold probe failed: %v (output=%q)", h.err, out)
		}
		t.Logf("hold probe ->\n%s", out)
		return out
	case <-time.After(time.Duration(fcHoldSeconds+90) * time.Second):
		t.Fatal("the hold probe never finished")
		return ""
	}
}

// TestMarkerWatcher_MarkerSplitAcrossChunks_StillFires pins the property the
// established-flow precondition rests on: the guest's stdout arrives as
// arbitrary frames, so the marker may be split across two of them. Matching
// per-chunk would silently never fire, and the suite would fall back to a 60s
// timeout instead of revoking on a live flow. Refs: MGIT-96
func TestMarkerWatcher_MarkerSplitAcrossChunks_StillFires(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{name: "whole_marker_in_one_chunk", chunks: []string{fcEstablishedMarker + " bytes=40\n"}, want: true},
		{name: "marker_split_across_chunks", chunks: []string{"PROBE-HOLD ", "ESTABLISHED bytes=40\n"}, want: true},
		{name: "marker_never_printed", chunks: []string{"PROBE-RESULT HOLD = CONNECTED-NO-DATA\n"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newMarkerWatcher(fcEstablishedMarker)
			for _, c := range tt.chunks {
				w.observe(c)
			}
			select {
			case <-w.seen:
				if !tt.want {
					t.Fatal("the watcher fired on a stream that never carried the marker")
				}
			default:
				if tt.want {
					t.Fatalf("the watcher did not fire on chunks %q", tt.chunks)
				}
			}
		})
	}
}

// TestMarkerWatcher_NilWatcher_IsANoOp guards the nil-watcher contract
// streamProbe relies on when a caller does not care about the marker.
func TestMarkerWatcher_NilWatcher_IsANoOp(t *testing.T) {
	var w *markerWatcher
	w.observe(fcEstablishedMarker) // must not panic
}
