package microvm

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// scriptedGuest answers every dial with one canned result frame, standing in
// for a guest built from another mgit release. reply runs once the request has
// been read, so the host's write never blocks on the pipe.
type scriptedGuest struct {
	frame          func() []byte // the result-frame JSON the guest sends; nil = say nothing
	hold           bool          // keep the connection open and silent
	dropAfterFirst bool          // answer the first dial (the probe), then close every later one unanswered
	dialed        int
}

func (g *scriptedGuest) DialGuest(ctx context.Context, _ string) (net.Conn, error) {
	g.dialed++
	drop := g.dropAfterFirst && g.dialed > 1
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		if _, err := execwire.ReadRequest(server); err != nil {
			return
		}
		if drop {
			return
		}
		if g.hold {
			<-ctx.Done()
			return
		}
		if g.frame != nil {
			_ = execwire.WriteFrame(server, execwire.FrameResult, g.frame())
		}
	}()
	return client, nil
}

// legacyRefusal is what a guest built before the handshake sends for the
// readiness probe: a refusal frame with no protocol and no build.
func legacyRefusal() []byte {
	b, _ := json.Marshal(map[string]any{
		"outcome": map[string]any{"exit_code": 0, "usage": map[string]any{}},
		"error":   "guest exec: start: no such file",
	})
	return b
}

func currentGuestFrame(protocol int, build string) func() []byte {
	return func() []byte {
		b, _ := json.Marshal(execwire.ResultFrame{
			Result: execwire.Result{GuestProtocol: protocol, GuestBuild: build},
			Error:  "guest exec: start: no such file",
		})
		return b
	}
}

// A new daemon in front of a guest that predates the handshake refuses the
// launch BY NAME: it says the guest is stale, names both builds' protocol
// state, and gives the recompose command. It never surfaces a bare EOF.
// Refs: MGIT-158
func TestLaunch_NewDaemonOldGuest_IsRefusedWithTheRecomposeHint(t *testing.T) {
	mgr := execManager(t, &scriptedGuest{frame: legacyRefusal})
	_, err := mgr.Launch(context.Background(), launchOpts("MGIT-158", model.NetworkModeNone))
	require.Error(t, err)
	require.ErrorIs(t, err, model.ErrGuestVersionSkew)
	assert.Contains(t, err.Error(), "mgit sandbox base from", "the message gives the recompose command")
	assert.NotContains(t, err.Error(), "EOF", "a stale guest is named, never reported as a dropped connection")
}

// A guest speaking a different protocol number is refused the same way, and
// the message names the guest's build so the operator can see which side is old.
func TestLaunch_GuestOnAnotherProtocol_IsRefusedAndNamesItsBuild(t *testing.T) {
	mgr := execManager(t, &scriptedGuest{frame: currentGuestFrame(execwire.ProtocolVersion+1, "v9.9.9")})
	_, err := mgr.Launch(context.Background(), launchOpts("MGIT-158", model.NetworkModeNone))
	require.ErrorIs(t, err, model.ErrGuestVersionSkew)
	assert.Contains(t, err.Error(), "v9.9.9")
	assert.Contains(t, err.Error(), "mgit sandbox base from")
}

// The guest is the hostile party: a build string it makes up is bounded and
// stripped of control characters before it reaches the operator's terminal.
func TestLaunch_HostileBuildString_IsBoundedInTheMessage(t *testing.T) {
	hostile := strings.Repeat("A", 100000) + "\x1b[31m"
	mgr := execManager(t, &scriptedGuest{frame: currentGuestFrame(execwire.ProtocolVersion+1, hostile)})
	_, err := mgr.Launch(context.Background(), launchOpts("MGIT-158", model.NetworkModeNone))
	require.ErrorIs(t, err, model.ErrGuestVersionSkew)
	assert.Less(t, len(err.Error()), 2048, "a made-up build string cannot flood the message")
	assert.NotContains(t, err.Error(), "\x1b", "control characters never reach the terminal")
}

// A guest that matches launches exactly as before.
func TestLaunch_MatchingGuest_LaunchesAsBefore(t *testing.T) {
	mgr := execManager(t, &scriptedGuest{frame: currentGuestFrame(execwire.ProtocolVersion, "v1.2.3")})
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-158", model.NetworkModeNone))
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)
}

// A guest that accepts the connection and says nothing cannot hang the
// launch: the probe's read is bounded even when the caller set no deadline.
func TestLaunch_SilentGuest_CannotHangTheHandshake(t *testing.T) {
	mgr := execManager(t, &scriptedGuest{hold: true})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := mgr.Launch(ctx, launchOpts("MGIT-158", model.NetworkModeNone))
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("a silent guest hung the launch past the readiness bound")
	}
}

// An old DAEMON in front of a new guest keeps working: the new fields ride in
// the result frame, and a decoder that does not know them ignores them. The
// legacy shape below is the frame struct as the daemon had it before.
func TestResultFrame_OldDaemonDecodesANewGuestsFrame(t *testing.T) {
	newFrame := currentGuestFrame(execwire.ProtocolVersion, "v1.2.3")()
	var legacy struct {
		Result struct {
			ExitCode int `json:"exit_code"`
		} `json:"outcome"`
		Error string `json:"error,omitempty"`
	}
	require.NoError(t, json.Unmarshal(newFrame, &legacy))
	assert.Equal(t, "guest exec: start: no such file", legacy.Error)
}

// After the launch proved the guest matches, a connection the guest drops
// mid-command is reported with a named suspect, not as a bare EOF: the guest
// process stopped, and version skew is ruled out because the launch checked it.
func TestExec_DroppedConnection_NamesTheSuspect(t *testing.T) {
	mgr := execManager(t, &scriptedGuest{
		frame: currentGuestFrame(execwire.ProtocolVersion, "v1.2.3"), dropAfterFirst: true,
	})
	info, err := mgr.Launch(context.Background(), launchOpts("MGIT-158", model.NetworkModeNone))
	require.NoError(t, err)
	_, err = mgr.Exec(context.Background(), info.ID, model.ExecRequest{Command: []string{"/bin/true"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "suspect")
	assert.Contains(t, err.Error(), "not version skew")
}
