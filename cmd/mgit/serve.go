package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	apihttp "github.com/hyper-swe/mgit/internal/api/http"
	mcpapp "github.com/hyper-swe/mgit/internal/mcp"
	gomcp "github.com/mark3labs/mcp-go/server"
)

// defaultServePort is the REST API port when --port is unset (FR-9.1).
const defaultServePort = 6860

// serveShutdownTimeout bounds the graceful REST shutdown so a hung
// connection cannot stall exit.
const serveShutdownTimeout = 10 * time.Second

// apiServer is the REST server lifecycle the serve command drives
// (*apihttp.Server satisfies it). Injected so serve is testable without a
// live socket.
type apiServer interface {
	Start(addr string) error
	Shutdown(ctx context.Context) error
}

// mcpRunner serves the MCP protocol until the context is canceled
// (*stdioMCP satisfies it via the mcp-go stdio transport).
type mcpRunner interface {
	Serve(ctx context.Context) error
}

// serveOptions is the resolved serve configuration.
type serveOptions struct {
	port     int
	startAPI bool
	startMCP bool
	asJSON   bool
}

// serveCmd starts the REST API and/or the MCP server, wiring the existing
// servers (internal/api/http, internal/mcp) into a CLI command with
// graceful shutdown on SIGINT/SIGTERM. Refs: FR-8.4, FR-9.1, FR-10.1
func serveCmd() *cobra.Command {
	var port int
	var apiOnly, mcpOnly, asJSON bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the REST API and/or MCP server (localhost-bound)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			startAPI, startMCP, err := resolveServeMode(apiOnly, mcpOnly)
			if err != nil {
				return err
			}
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			// A long-lived server must NOT hold the exclusive repo lock for its
			// lifetime — that starves every CLI command on the same repo (MGIT-46).
			// Detach the lifetime lock and switch to per-operation guarding: each
			// REST request / MCP tool call acquires the lock only for its duration,
			// so serve and the CLI (and concurrent server requests) interleave.
			locker := app.DetachLock()

			clock := func() time.Time { return time.Now().UTC() }
			// SIGINT/SIGTERM trigger graceful shutdown.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			api := apihttp.NewServer(app.Repo, app.Index, clock, apihttp.WithLocker(locker))
			mcp := stdioMCP{srv: mcpapp.NewServer(app.Repo, app.Index, mcpapp.WithLocker(locker)).MCPServer()}
			return runServe(ctx, api, mcp, serveOptions{
				port: port, startAPI: startAPI, startMCP: startMCP, asJSON: asJSON,
			}, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().IntVar(&port, "port", defaultServePort, "REST API port")
	cmd.Flags().BoolVar(&apiOnly, "api-only", false, "start only the REST API")
	cmd.Flags().BoolVar(&mcpOnly, "mcp-only", false, "start only the MCP server (stdio)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit structured startup info (to stderr)")
	return cmd
}

// resolveServeMode maps the --api-only/--mcp-only flags to which servers
// run. The two flags are mutually exclusive; with neither, both run.
func resolveServeMode(apiOnly, mcpOnly bool) (startAPI, startMCP bool, err error) {
	if apiOnly && mcpOnly {
		return false, false, fmt.Errorf("--api-only and --mcp-only are mutually exclusive")
	}
	if apiOnly {
		return true, false, nil
	}
	if mcpOnly {
		return false, true, nil
	}
	return true, true, nil
}

// apiAddr is the localhost bind address for the REST API. Binding is
// localhost-only by default (security: never 0.0.0.0). Refs: NFR-5
func apiAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// runServe starts the selected servers and blocks until the context is
// canceled (signal) or a server fails fatally, then gracefully shuts the
// REST server down. Startup info goes to out (stderr) so it never corrupts
// the MCP stdio JSON-RPC stream on stdout. Refs: FR-8.4, FR-9.1, FR-10.1
func runServe(ctx context.Context, api apiServer, mcp mcpRunner, opts serveOptions, out io.Writer) error {
	errc := make(chan error, 2)
	if opts.startAPI {
		addr := apiAddr(opts.port)
		announceServe(out, opts, addr)
		go func() {
			if err := api.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("rest api: %w", err)
			}
		}()
	}
	if opts.startMCP {
		go func() {
			if err := mcp.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errc <- fmt.Errorf("mcp server: %w", err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	if opts.startAPI {
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
		defer cancel()
		if err := api.Shutdown(shutCtx); err != nil && runErr == nil {
			runErr = fmt.Errorf("rest api shutdown: %w", err)
		}
	}
	return runErr
}

// announceServe reports what started, to stderr (stdout is reserved for the
// MCP protocol stream).
func announceServe(out io.Writer, opts serveOptions, addr string) {
	mode := serveModeLabel(opts)
	if opts.asJSON {
		_ = json.NewEncoder(out).Encode(map[string]any{
			"status": "started", "mode": mode, "api_addr": addr,
		})
		return
	}
	_, _ = fmt.Fprintf(out, "mgit serve: REST API on %s (mode: %s)\n", addr, mode)
}

// serveModeLabel describes the running server set.
func serveModeLabel(opts serveOptions) string {
	switch {
	case opts.startAPI && opts.startMCP:
		return "api+mcp"
	case opts.startAPI:
		return "api"
	default:
		return "mcp"
	}
}

// stdioMCP serves the MCP server over the stdio transport, honoring ctx for
// graceful shutdown.
type stdioMCP struct{ srv *gomcp.MCPServer }

// Serve runs the stdio MCP loop until ctx is canceled.
func (m stdioMCP) Serve(ctx context.Context) error {
	return gomcp.NewStdioServer(m.srv).Listen(ctx, os.Stdin, os.Stdout)
}
