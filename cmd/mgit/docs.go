package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/docs"
	mcpapp "github.com/hyper-swe/mgit/internal/mcp"
)

// mcpToolInfos maps the MCP server's live registered tool set to the docs
// generator's input type, so `mgit docs generate` documents exactly the tools
// the server serves (no drift). Refs: MGIT-50
func mcpToolInfos(srv *mcpapp.Server) []docs.MCPToolInfo {
	tds := srv.ToolDocs()
	infos := make([]docs.MCPToolInfo, 0, len(tds))
	for _, td := range tds {
		infos = append(infos, docs.MCPToolInfo{
			Name: td.Name, Description: td.Description, Parameters: td.Parameters,
		})
	}
	return infos
}

// docsCmd implements mgit docs generate. Refs: FR-15, MGIT-7.3.1
func docsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Generate agent-facing documentation",
	}

	var force bool
	genCmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate all documentation files",
		RunE: func(_ *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			outDir := filepath.Join(cwd, "docs")
			clock := func() time.Time { return time.Now().UTC() }

			// Derive the MCP tool reference from the LIVE registered tool set, so
			// the generated docs can never drift from what the server serves
			// (MGIT-50). This runs in the repo whose docs are being generated.
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()
			mcpTools := mcpToolInfos(mcpapp.NewServer(app.Repo, app.Index))

			gen := docs.NewGenerator(outDir, rootCmd(), mcpTools, Version, clock)
			results, err := gen.Generate(force)
			if err != nil {
				return fmt.Errorf("generate docs: %w", err)
			}

			for _, r := range results {
				_, _ = fmt.Fprintf(os.Stdout, "%-25s %s\n", r.File, r.Action)
			}
			_, _ = fmt.Fprintf(os.Stdout, "\n%d files processed in %s\n", len(results), outDir)
			return nil
		},
	}

	genCmd.Flags().BoolVar(&force, "force", false, "Force regenerate all files")
	cmd.AddCommand(genCmd)
	return cmd
}
