//go:build !(cgo && !vzf && (darwin || (linux && libkrun)))

package main

import (
	"log/slog"

	"github.com/hyper-swe/mgit/internal/service"
)

// platformPolicyController reports NO backend-specific live-policy enforcer.
//
// The daemon then falls back to whatever the host egress wiring produced (the
// firecracker runner on Linux) and, failing that, leaves the policy verbs
// UNSERVED — which the CLI and MCP surface report as an error. That is
// deliberate: a build with nothing enforcing must never answer a revoke with
// success. Refs: MGIT-72, SEC-04
func platformPolicyController(_ string, _ *slog.Logger) service.EgressPolicyController {
	return nil
}
