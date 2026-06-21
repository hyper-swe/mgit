package egress

import (
	"errors"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// Package egress denial sentinels all wrap model.ErrNetworkPolicyViolation
// so a caller can classify any policy refusal with one errors.Is, while
// the wrapped message records why for the audit trail. Refs: FR-17.8
var (
	// ErrNameNotAllowlisted is returned when the guest asks to resolve a
	// name that is not on the allowlist (SEC-07: non-allowlisted names do
	// not resolve, throttling DNS-label exfiltration).
	ErrNameNotAllowlisted = fmt.Errorf("%w: name not allowlisted", model.ErrNetworkPolicyViolation)
	// ErrRateLimited is returned when the per-sandbox DNS query rate cap is
	// exceeded (SEC-07 anti-tunnel).
	ErrRateLimited = fmt.Errorf("%w: dns query rate exceeded", model.ErrNetworkPolicyViolation)
	// ErrEgressDenied is returned when a TCP flow is refused by the proxy
	// (non-allowlisted IP, denied range, raw-IP bypass, or non-TCP).
	ErrEgressDenied = fmt.Errorf("%w: egress denied", model.ErrNetworkPolicyViolation)
)

// ErrNXDOMAIN marks a name that resolved to no records. The production
// LookupFunc maps a not-found DNS error to this sentinel so the resolver
// can count NXDOMAIN bursts (SEC-07) without depending on net error
// internals. Refs: SEC-07
var ErrNXDOMAIN = errors.New("nxdomain")
