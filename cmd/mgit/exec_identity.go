package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
)

// bindAsRootFlag adds the one escalation a guest exec has: `--as-root`
// asks the daemon for root inside the guest, which it audits. Without it a
// command runs as the daemon's own user — the identity the worktree and
// the base were delivered as. Refs: MGIT-151
func bindAsRootFlag(cmd *cobra.Command, asRoot *bool) {
	cmd.Flags().BoolVar(asRoot, "as-root", false,
		"run the command as root inside the guest (audited); the default is the daemon's own user")
}

// writeExecIdentity says, after the command's own output, when the daemon
// could not verify the identity the command ran as: the guest did not
// report one (a base composed before this version runs commands as root)
// or reported another. A verified identity, or no verdict from an older
// daemon, prints nothing. Refs: MGIT-151
func writeExecIdentity(w io.Writer, id *model.ExecIdentity) {
	if id == nil || id.Verified {
		return
	}
	fmt.Fprintf(w, "mgit: guest exec identity UNVERIFIED — %s\n", id.Reason)
}
