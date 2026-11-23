package guest

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/execwire"
	"github.com/hyper-swe/mgit/internal/model"
)

func TestGuestPipesStdinToTheChildAndLogsNothingOfIt(t *testing.T) {
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	const secret = "FAKE-KEY-0123456789-not-real"
	var logs bytes.Buffer
	sup := NewSupervisor(slog.New(slog.NewJSONHandler(&logs, nil)))
	var req bytes.Buffer
	require.NoError(t, execwire.WriteRequest(&req, model.ExecRequest{Command: []string{cat}, Stdin: model.Secret(secret)}))
	var resp bytes.Buffer
	require.NoError(t, sup.Serve(context.Background(), rwPair{Reader: &req, Writer: &resp}))
	stdout, _, outcome := readResponse(t, &resp)
	assert.Equal(t, secret, stdout, "the child read its stdin")
	assert.Zero(t, outcome.ExitCode)
	assert.False(t, strings.Contains(logs.String(), secret), "the guest log carries the secret: %s", logs.String())
}

func TestAChildWithNoStdinSeesAClosedInput(t *testing.T) {
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	sup := NewSupervisor(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	var req bytes.Buffer
	require.NoError(t, execwire.WriteRequest(&req, model.ExecRequest{Command: []string{cat}}))
	var resp bytes.Buffer
	require.NoError(t, sup.Serve(context.Background(), rwPair{Reader: &req, Writer: &resp}))
	stdout, _, _ := readResponse(t, &resp)
	assert.Empty(t, stdout)
}
