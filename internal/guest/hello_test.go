package guest

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// Every result frame the guest sends names the wire protocol it speaks and its
// own build, so the daemon can tell a matching guest from a stale one at the
// first exchange. A refusal frame carries it too: the readiness probe is
// answered with a refusal, and that is the frame the daemon reads.
// Refs: MGIT-158
func TestServe_EveryResultFrameNamesTheGuestsProtocolAndBuild(t *testing.T) {
	sup := testSupervisor(t)
	cases := map[string]model.ExecRequest{
		"a command that cannot start": {Command: []string{"/nonexistent/mgit-guest-readiness-probe"}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			var in bytes.Buffer
			require.NoError(t, execwire.WriteRequest(&in, req))
			var resp bytes.Buffer
			_ = sup.Serve(context.Background(), rwPair{Reader: &in, Writer: &resp})
			_, _, outcome := readResponse(t, &resp)
			assert.Equal(t, execwire.ProtocolVersion, outcome.GuestProtocol)
			assert.NotEmpty(t, outcome.GuestBuild)
		})
	}
}
