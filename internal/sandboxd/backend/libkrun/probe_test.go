package libkrun

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// The probe must run where a VM boots: in a re-exec'd child with the VM
// child's own environment. In the daemon's process a stock Mac cannot load
// libkrunfw at all (Homebrew's lib dir is not on dyld's default fallback
// path), so an in-process probe would report a problem the VM child never
// has. Refs: MGIT-229, MGIT-61.15
func TestProbeCmd_RunsTheProbeSubcommandWithTheVMChildsEnvironment(t *testing.T) {
	cmd := probeCmd(t.Context(), "/opt/mgit/bin/mgit-sandboxd")
	assert.Equal(t, []string{"/opt/mgit/bin/mgit-sandboxd", ProbeCommand}, cmd.Args)
	assert.Equal(t, childEnv(os.Getenv, libkrunfwDirs), cmd.Env,
		"the probe child must see exactly what a VM child sees")
}

func TestDescribeFromProbe_RelaysOrNamesTheFailure(t *testing.T) {
	good := `{"vmm":"libkrun","libraries":[{"name":"libkrun","path":"/l/libkrun.so.1"},{"name":"libkrunfw","path":"/l/libkrunfw.so.5"}]}`
	tests := []struct {
		name        string
		stdout      string
		stderr      string
		err         error
		wantCanBoot bool
		wantMention []string
	}{
		{name: "report_relayed", stdout: good, wantCanBoot: true},
		{
			name:        "loader_refused_the_child",
			stderr:      "mgit-sandboxd: error while loading shared libraries: libkrun.so.1: cannot open shared object file: No such file or directory",
			err:         errors.New("exit status 127"),
			wantMention: []string{"libkrun.so.1: cannot open shared object file", "exit status 127"},
		},
		{name: "said_nothing", err: nil, wantMention: []string{"printed no report"}},
		{name: "not_json", stdout: "hello", wantMention: []string{"could not be read", "hello"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := describeFromProbe([]byte(tt.stdout), []byte(tt.stderr), tt.err)
			assert.Equal(t, model.BackendLibkrun, r.VMM)
			assert.Equal(t, tt.wantCanBoot, r.CanBoot(), "problems: %q", r.Problems)
			all := strings.Join(r.Problems, "\n")
			for _, want := range tt.wantMention {
				assert.Contains(t, all, want)
			}
		})
	}
}

// The child's stderr is where a loader failure is written, and it can be
// long; the report keeps its END, where the reason is.
func TestDescribeFromProbe_KeepsTheTailOfALongStderr(t *testing.T) {
	long := strings.Repeat("noise line\n", 500) + "the reason is here"
	r := describeFromProbe(nil, []byte(long), errors.New("exit status 1"))
	require.Len(t, r.Problems, 1)
	assert.Contains(t, r.Problems[0], "the reason is here")
	assert.Less(t, len(r.Problems[0]), 2000)
}
