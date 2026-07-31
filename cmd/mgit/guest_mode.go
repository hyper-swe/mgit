package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/guest"
)

// guestModeEnv marks a process running INSIDE an mgit sandbox guest. The
// in-guest PID-1 supervisor (mgit-guest) puts it in the clean base
// environment every guest child starts from, so it reaches the agent's shell
// and any mgit the agent runs there.
//
// It is a USABILITY control, not a containment one — be precise about that.
// Containment is the VM boundary: the host's daemon socket, shared store and
// worktree simply are not present in the guest, so a host-only command cannot
// reach them whether or not this marker is set. What the marker buys is a
// diagnosis ("run this on the host") instead of a connection error, and it is
// therefore fine that a guest process can unset it.
//
// Single-sourced from the supervisor that WRITES it, so the writer and the
// reader cannot drift. Refs: MGIT-61.7, FR-17.11
const guestModeEnv = guest.GuestModeEnv

// inSandboxGuest reports whether this mgit is running inside a sandbox guest.
// Any non-empty value counts: the supervisor is the only writer, and a
// stricter parse would add a way to be subtly wrong with no benefit.
func inSandboxGuest() bool { return os.Getenv(guestModeEnv) != "" }

// hostOnly marks a command that drives the HOST — the sandbox daemon, the
// host's agent shell, or the host's worktree registry — and wraps its
// RunE so that inside a guest it refuses with the reason instead of failing
// on an absent socket or path. The gate is applied to the command AND every
// subcommand, so a new `mgit sandbox <verb>` is covered by construction
// rather than by remembering to mark it.
//
// The checkpoint commands (commit/status/log/diff/add/branch/squash) are
// deliberately NOT gated: running them in the guest, against the SEC-03
// private store, is the entire reason mgit ships in the guest image.
// Refs: MGIT-61.7, SEC-03
func hostOnly(cmd *cobra.Command) *cobra.Command {
	refuse := func(c *cobra.Command) error {
		return fmt.Errorf(
			"%s cannot run inside the mgit sandbox: it drives the host "+
				"(the sandbox daemon, the agent shell, or the host worktree registry), "+
				"which the guest deliberately cannot reach. Run it on the host instead; "+
				"in-guest, use mgit commit/status/log/diff against this sandbox's own store",
			c.CommandPath())
	}
	var gate func(c *cobra.Command)
	gate = func(c *cobra.Command) {
		if run := c.RunE; run != nil {
			c.RunE = func(cc *cobra.Command, args []string) error {
				if inSandboxGuest() {
					return refuse(cc)
				}
				return run(cc, args)
			}
		}
		if run := c.Run; run != nil {
			c.Run = nil
			c.RunE = func(cc *cobra.Command, args []string) error {
				if inSandboxGuest() {
					return refuse(cc)
				}
				run(cc, args)
				return nil
			}
		}
		for _, sub := range c.Commands() {
			gate(sub)
		}
	}
	gate(cmd)
	return cmd
}
