package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/model"
)

// sandboxExportCmd brings a guest-built artifact OUT of a task's sandbox onto
// the host, so a provisioning cache (node_modules, a build cache) survives the
// round instead of being rebuilt every time.
//
// It is the deliberate, audited counterpart to land: land is the verified
// bridge for COMMITTED OBJECTS into the shared store; export is a bridge for
// FILES into a host-named destination, and it never touches the git store.
// Both are host-initiated — the guest names neither the source nor the
// destination, because a guest-chosen destination would be a host-filesystem
// write primitive.
//
// The refusals are the feature, so they are stated in the help text: an
// escaping symlink, a hardlink out of the subtree, a traversing path, the
// sandbox's private store, an existing destination, or a transfer past the
// size/file ceilings all fail CLOSED with nothing written.
// Refs: MGIT-73, SEC-03, SEC-10, ADR-011
func sandboxExportCmd(connect connectFunc) *cobra.Command {
	var task string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "export --task <id> <guest-path> <host-path>",
		Short: "Export a guest-built artifact from a task's sandbox to a host path",
		Long: "Copy <guest-path> (relative to the sandbox worktree) out to <host-path> on the host.\n\n" +
			"Both paths are named by YOU, on the host; the guest never chooses either.\n" +
			"The export is refused, with nothing written, if the subtree contains a symlink\n" +
			"or hardlink leaving it, if <guest-path> traverses out of the worktree or names\n" +
			"the sandbox's private .mgit store, if <host-path> already exists (collisions are\n" +
			"never overwritten), or if the transfer exceeds its size or file-count ceiling.\n\n" +
			"Every export lands with a provenance sidecar (<host-path>.mgit-export.json)\n" +
			"naming the sandbox, task, base image digest and per-file hashes, and is recorded\n" +
			"in the append-only audit trail.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if task == "" {
				return fmt.Errorf("--task-id is required")
			}
			cl, err := connect(cmd.Context())
			if err != nil {
				return err
			}
			// The host destination is resolved to an absolute path HERE, so the
			// daemon is never handed something whose meaning depends on its
			// working directory. The guest path stays verbatim: it is
			// worktree-relative and the backend validates it against the staged
			// tree.
			res, err := cl.ExportArtifact(cmd.Context(), task,
				model.ArtifactExportRequest{GuestPath: args[0], HostPath: canonicalPath(args[1])})
			if err != nil {
				return err
			}
			return writeExportResult(cmd.OutOrStdout(), res, asJSON)
		},
	}
	bindTaskIDFlag(cmd, &task, "task ID whose sandbox holds the artifact (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

// writeExportResult renders a completed export as JSON or a human summary.
func writeExportResult(w io.Writer, res *model.ArtifactExportResult, asJSON bool) error {
	if res == nil {
		res = &model.ArtifactExportResult{}
	}
	if asJSON {
		return json.NewEncoder(w).Encode(res)
	}
	_, _ = fmt.Fprintf(w, "Exported %s -> %s (%d file(s), %d bytes)\n",
		res.GuestPath, res.HostPath, res.Files, res.Bytes)
	_, _ = fmt.Fprintf(w, "Provenance: %s (tree %s)\n", res.ManifestPath, res.TreeHash)
	return nil
}
