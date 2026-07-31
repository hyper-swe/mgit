package main

import (
	"bytes"
	"strings"
	"testing"
)

// The mgit CLI ships INSIDE the sandbox guest image (MGIT-61.7), because
// `mgit run` routes the agent's shell into the microVM — so `mgit commit`
// executes in the guest, against the SEC-03 private store. These tests pin
// which commands remain usable there and which refuse with a reason.
// Refs: MGIT-61.7, SEC-03, FR-17.11

func TestGuestMode_HostOnlyCommands_RefuseInsideTheSandbox(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		// Every one of these drives the HOST sandbox daemon or the host's
		// agent shell. Inside the guest there is no daemon socket and no host
		// worktree; running them would either recurse into a nested sandbox
		// or fail with an unrelated connection error.
		{name: "sandbox_launch", args: []string{"sandbox", "launch"}},
		{name: "sandbox_land", args: []string{"sandbox", "land"}},
		{name: "sandbox_image_install", args: []string{"sandbox", "image", "install"}},
		{name: "run", args: []string{"run", "--", "echo", "hi"}},
		{name: "serve", args: []string{"serve"}},
		{name: "work", args: []string{"work", "/tmp/wt", "--task", "MGIT-1"}},
		{name: "worktree_add", args: []string{"worktree", "add", "/tmp/wt", "--task", "MGIT-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(guestModeEnv, "1")

			var out bytes.Buffer
			root := rootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tt.args)

			err := root.Execute()
			if err == nil {
				t.Fatalf("%v succeeded inside the sandbox guest; it drives the host and must refuse",
					tt.args)
			}
			// The message must say WHERE to run it, not just "failed".
			for _, want := range []string{"inside the mgit sandbox", "host"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q — an agent needs to know to run it on the host", err, want)
				}
			}
		})
	}
}

func TestGuestMode_WorkingCommands_AreNotRefused(t *testing.T) {
	// The whole point of shipping mgit in the guest: the checkpoint loop
	// works there, against the private store. These must not be gated. They
	// still fail here (no repository in the test's cwd) — the assertion is
	// that they fail for THAT reason, not because they were refused.
	tests := []struct{ name, arg string }{
		{name: "commit", arg: "commit"},
		{name: "status", arg: "status"},
		{name: "log", arg: "log"},
		{name: "diff", arg: "diff"},
		{name: "add", arg: "add"},
		{name: "branch", arg: "branch"},
		{name: "squash", arg: "squash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(guestModeEnv, "1")
			t.Chdir(t.TempDir()) // no repository here

			var out bytes.Buffer
			root := rootCmd()
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{tt.arg})

			if err := root.Execute(); err != nil && strings.Contains(err.Error(), "inside the mgit sandbox") {
				t.Fatalf("%s is refused in the guest, but it is exactly what agents run there: %v", tt.arg, err)
			}
		})
	}
}

func TestGuestMode_OffByDefault_HostOnlyCommandsStillRun(t *testing.T) {
	// Without the marker (i.e. on the host) the gate must be invisible.
	t.Setenv(guestModeEnv, "")

	var out bytes.Buffer
	root := rootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"sandbox", "list"})

	// It may fail (no daemon in a test env) but never with the guest refusal.
	if err := root.Execute(); err != nil && strings.Contains(err.Error(), "inside the mgit sandbox") {
		t.Fatalf("host run was refused as if in a guest: %v", err)
	}
}

func TestInSandboxGuest_ReadsTheMarkerEnv(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "set_to_1", value: "1", want: true},
		{name: "empty", value: "", want: false},
		// Any non-empty value counts: the guest supervisor sets it, and a
		// stricter parse would only add a way to get it subtly wrong.
		{name: "any_value", value: "true", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(guestModeEnv, tt.value)
			if got := inSandboxGuest(); got != tt.want {
				t.Errorf("inSandboxGuest() = %v, want %v", got, tt.want)
			}
		})
	}
}
