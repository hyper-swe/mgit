// Package guestexec is the host side of the exec channel (FR-17.11): it
// sends one ExecRequest to a running guest over an already-dialed
// connection and streams the guest's stdout/stderr back as they are
// produced, returning the exit code unchanged. It is transport-agnostic
// (any io.ReadWriter — a vsock conn in production, an in-memory pipe in
// tests) and speaks the single-sourced internal/execwire protocol, so the
// host and guest framings cannot drift. The guest is the hostile party;
// the wire reader enforces per-frame ceilings (execwire.ReadFrame).
// Refs: FR-17.11
package guestexec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// ErrGuestReported marks an error the GUEST itself sent back on the wire — a
// command it could not start, for instance — as opposed to a transport failure
// where the guest was never reached. Refs: MGIT-92, FR-17.11
var ErrGuestReported = errors.New("guest exec")

// Run sends req over conn, streams the guest's stdout/stderr to the given
// writers as frames arrive, and returns the terminal result. A non-zero
// exit code is a normal result (returned with a nil error); only a
// transport failure or a guest-reported start failure is an error. The
// caller owns conn's lifecycle (and any deadline). Refs: FR-17.11
func Run(conn io.ReadWriter, req model.ExecRequest, stdout, stderr io.Writer) (execwire.Result, error) {
	if err := req.Validate(); err != nil {
		return execwire.Result{}, fmt.Errorf("guest exec: %w", err)
	}
	if err := execwire.WriteRequest(conn, req); err != nil {
		return execwire.Result{}, fmt.Errorf("guest exec: send request: %w", err)
	}
	for {
		kind, payload, err := execwire.ReadFrame(conn)
		if err != nil {
			return execwire.Result{}, fmt.Errorf("guest exec: read frame: %w", err)
		}
		switch kind {
		case execwire.FrameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return execwire.Result{}, fmt.Errorf("guest exec: write stdout: %w", err)
			}
		case execwire.FrameStderr:
			if _, err := stderr.Write(payload); err != nil {
				return execwire.Result{}, fmt.Errorf("guest exec: write stderr: %w", err)
			}
		case execwire.FrameResult:
			return decodeResult(payload)
		default:
			return execwire.Result{}, fmt.Errorf("guest exec: unknown frame kind %q", kind)
		}
	}
}

// decodeResult parses the terminal result frame. A guest-reported error
// (a failure to start the child) surfaces as an error alongside the
// outcome; a clean non-zero exit does not. Refs: FR-17.11
func decodeResult(payload []byte) (execwire.Result, error) {
	var frame execwire.ResultFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return execwire.Result{}, fmt.Errorf("guest exec: decode result: %w", err)
	}
	if frame.Error != "" {
		// Wrapped in ErrGuestReported so a caller can tell "the guest answered
		// and refused" from "the guest was never reached" WITHOUT parsing
		// message text. Readiness turns on exactly that distinction: a refusal
		// proves the control plane is serving, a dial failure proves the
		// opposite. Refs: MGIT-92, FR-17.11
		return frame.Result, fmt.Errorf("%w: %s", ErrGuestReported, frame.Error)
	}
	return frame.Result, nil
}
