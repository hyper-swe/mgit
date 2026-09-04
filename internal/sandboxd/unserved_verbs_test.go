package sandboxd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// firecrackerStatus answers every status lookup with a firecracker sandbox,
// so a refusal can be checked for the backend fact it must name.
type firecrackerStatus struct{ *fakeDispatcher }

func (firecrackerStatus) Status(context.Context, string) (*model.SandboxInfo, error) {
	return &model.SandboxInfo{ID: "01FIRECRACKER", TaskID: "MGIT-1", Backend: "firecracker"}, nil
}

// An unwired export on firecracker names the REAL reason nothing can leave:
// the worktree was delivered as a launch-time image, so there is no host
// directory to read an artifact from — a property of the backend, not of the
// daemon's wiring, and the one fact that tells the operator to re-launch
// elsewhere rather than retry. Refs: MGIT-171, MGIT-73
func TestClient_ExportRefusal_OnFirecracker_NamesTheLaunchTimeImage(t *testing.T) {
	client := clientFor(t, func(c *Config) { c.Service = firecrackerStatus{&fakeDispatcher{}} })
	_, err := client.ExportArtifact(context.Background(), "MGIT-1",
		model.ArtifactExportRequest{GuestPath: "dist/app", HostPath: "/host/out/app"})
	require.Error(t, err)
	msg := err.Error()
	for _, want := range []string{
		"`mgit sandbox export` is not served by this daemon on firecracker",
		"launch-time image", // the backend fact
		"mgit sandbox land", // what to use instead
		"nothing left the sandbox",
		"nothing was changed",
	} {
		assert.Contains(t, msg, want)
	}
	assert.NotRegexp(t, `kind 0x[0-9a-f]+`, msg)
}

// Each refusal states, for its own verb, what did NOT happen — the fact a
// caller needs before deciding whether there is anything to undo.
// Refs: MGIT-171
func TestClient_UnservedVerbs_EachSaysWhatDidNotHappen(t *testing.T) {
	client := clientFor(t, func(*Config) {})
	ctx := context.Background()
	tests := []struct {
		verb string
		call func() error
		want string
	}{
		{"land", func() error { _, err := client.Land(ctx, "MGIT-1"); return err }, "nothing was landed"},
		{"grants", func() error { _, err := client.Grants(ctx, "MGIT-1"); return err }, "no request was listed"},
		{"grant", func() error { _, err := client.Grant(ctx, "MGIT-1", "k"); return err }, "no egress was opened"},
		{"export", func() error {
			_, err := client.ExportArtifact(ctx, "MGIT-1", model.ArtifactExportRequest{GuestPath: "a", HostPath: "/b"})
			return err
		}, "nothing left the sandbox"},
	}
	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			err := tt.call()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "`mgit sandbox "+tt.verb+"` is not served by this daemon")
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
