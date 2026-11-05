package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const guestHandshakeRefusal = "sandbox ensure-running: libkrun launch: guest is from a different mgit build than the daemon: the guest predates the version handshake, the daemon is build current; recompose this repository's guest base with `mgit sandbox base from <image>`"

func TestGuestHandshakeRefusalAdvisory(t *testing.T) {
	for name, err := range map[string]error{
		"sentinel":     fmt.Errorf("launch: %w", model.ErrGuestVersionSkew),
		"relayed":      errors.New(guestHandshakeRefusal),
		"console_tail": errors.New(guestHandshakeRefusal + "\nguest console (tail): connection refused; EOF"),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			writeGuestFailureAdvisory(t.Context(), &out, advisoryInfo(), err)
			assert.Empty(t, out.String(), "the named refusal already carries the recovery; no second diagnosis")
		})
	}
	var out bytes.Buffer
	writeGuestFailureAdvisory(t.Context(), &out, advisoryInfo(), errors.New("an unknown handshake failure"))
	assert.Contains(t, out.String(), "could not identify", "do not suppress errors just because they mention a handshake")
}

// Drive the real CLI and control socket with a fixture result frame. This tests
// CLI formatting, not another VM boot or guest protocol implementation.
func TestGuestHandshakeRealCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandbox control socket is Unix-only")
	}
	bin := buildMgitTestBinary(t)
	for _, tc := range []struct {
		name, failure string
		unidentified  bool
	}{
		{"old_guest", guestHandshakeRefusal, false},
		{"other_protocol", "libkrun launch: " + model.ErrGuestVersionSkew.Error() + ": the guest speaks exec protocol 99 (this daemon speaks 1); recompose this repository's guest base with `mgit sandbox base from <image>`", false},
		{"unknown", "an unknown handshake failure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(t)
			dir, err := os.MkdirTemp("/tmp", "ghs")
			require.NoError(t, err)
			t.Setenv("XDG_RUNTIME_DIR", dir)
			paths, err := resolveSandboxPaths(repo)
			require.NoError(t, err)
			t.Cleanup(func() {
				assert.NoError(t, os.Remove(filepath.Dir(paths.socket)))
				assert.NoError(t, os.Remove(filepath.Dir(filepath.Dir(paths.socket))))
				assert.NoError(t, os.Remove(dir))
			})
			ln, err := net.Listen("unix", paths.socket)
			require.NoError(t, err)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					err = guestHandshakeFixture(conn, repo, tc.failure)
					_ = conn.Close()
					if err != nil {
						t.Errorf("fixture: %v", err)
					}
				}
			}()
			t.Cleanup(func() { _ = ln.Close(); <-done })
			for _, args := range [][]string{
				{"sandbox", "exec", "--task-id", "CHECK-1", "--", "true"},
				{"run", "--", "true"},
			} {
				cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // test-built CLI, literal arguments
				cmd.Dir = repo
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit, "%s", output)
				assert.Equal(t, 1, exit.ExitCode(), "%s", output)
				assert.Contains(t, string(output), tc.failure)
				if tc.unidentified {
					assert.Contains(t, string(output), "could not identify")
				} else {
					assert.NotContains(t, string(output), "could not identify")
					assert.NotContains(t, string(output), "stopped answering mid-command")
					assert.Contains(t, string(output), "mgit sandbox base from <image>")
				}
			}
		})
	}
}

func guestHandshakeFixture(conn net.Conn, repo, failure string) error {
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "ok mgit-sandboxd\n"); err != nil {
		return err
	}
	hello, err := controlproto.ReadRequest(conn)
	if errors.Is(err, io.EOF) {
		return nil
	} // activation only reads the greeting
	if err != nil {
		return err
	}
	if hello.Kind != controlproto.KindHello {
		return fmt.Errorf("expected hello, got %q", hello.Kind)
	}
	if err := controlproto.WriteResponse(conn, &controlproto.Response{Hello: &controlproto.HelloResult{Protocol: controlproto.ProtocolVersion, Version: "fixture"}}); err != nil {
		return err
	}
	req, err := controlproto.ReadRequest(conn)
	if err != nil {
		return err
	}
	info := model.SandboxInfo{ID: "fixture", TaskID: "CHECK-1", WorktreePath: canonicalPath(repo), State: model.StateCreated, MemoryMB: 1024, CPUs: 1}
	switch req.Kind {
	case controlproto.KindExec:
		data, err := json.Marshal(execwire.ResultFrame{Error: failure})
		if err != nil {
			return err
		}
		return execwire.WriteFrame(conn, execwire.FrameResult, data)
	case controlproto.KindStatus:
		return controlproto.WriteResponse(conn, &controlproto.Response{Sandbox: &info})
	case controlproto.KindList:
		return controlproto.WriteResponse(conn, &controlproto.Response{List: []model.SandboxInfo{info}})
	default:
		return fmt.Errorf("unexpected fixture request %q", req.Kind)
	}
}
