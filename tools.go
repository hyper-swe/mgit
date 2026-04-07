//go:build tools

// Package mgit pins tool and indirect dependencies that are not yet imported
// in production code but are approved per APPROVED-PACKAGES.md.
// This file ensures go mod tidy does not remove them prematurely.
// Refs: NFR-4
package mgit

import (
	_ "github.com/go-git/go-git/v5"
	_ "github.com/labstack/echo/v4"
	_ "github.com/mark3labs/mcp-go/mcp"
	_ "github.com/oklog/ulid/v2"
	_ "github.com/spf13/cobra"
	_ "golang.org/x/crypto/ed25519"
	_ "golang.org/x/sync/errgroup"
	_ "modernc.org/sqlite"
)
