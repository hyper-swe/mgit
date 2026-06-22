package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// GuestCredentialSink injects per-session credentials into a confined-agent
// (T2) guest over the control plane. The secret values cross to the guest
// only — never to host disk or the audit log. Implemented host-side by the
// daemon's guest channel. Refs: ADR-005, MGIT-11.11.4
type GuestCredentialSink interface {
	InjectCredentials(ctx context.Context, sandboxID, sessionID string, creds []model.SessionCredential) error
}

// ConfinedSessionService orchestrates T2 attached sessions (ADR-005,
// "Fully confined agent"). It gates on the opt-in confine_agent policy,
// injects per-session credentials into the guest, and appends an
// append-only audit event flagging THAT credentials were injected — by
// NAME only, never the secret value (so the audit trail records the access
// without becoming a secret store). Refs: FR-17.18, MGIT-11.11.4
type ConfinedSessionService struct {
	sink   GuestCredentialSink
	events SandboxEventAppender
	clock  func() time.Time
	newID  func() (string, error)
}

// NewConfinedSessionService wires the service with injected dependencies.
// All are required. Refs: MGIT-11.11.4
func NewConfinedSessionService(sink GuestCredentialSink, events SandboxEventAppender, clock func() time.Time, newID func() (string, error)) (*ConfinedSessionService, error) {
	if sink == nil || events == nil || clock == nil || newID == nil {
		return nil, fmt.Errorf("confined session service: sink, events, clock and newID are required")
	}
	return &ConfinedSessionService{sink: sink, events: events, clock: clock, newID: newID}, nil
}

// Start begins a T2 attached session: it refuses unless confineAgent is set
// (ErrConfineAgentDisabled — T2 is strictly opt-in), mints a session ID,
// injects the per-session credentials into the guest, then records a
// credentials_injected audit event carrying the credential NAMES only.
// Injection precedes the audit so a failed injection is never falsely
// flagged; an audit failure after a successful injection aborts the session
// (the daemon tears the guest down — no usable but unaudited session).
// Returns the session ID. Refs: ADR-005, MGIT-11.11.4
func (s *ConfinedSessionService) Start(ctx context.Context, sandboxID, taskID string, confineAgent bool, creds []model.SessionCredential) (string, error) {
	if !confineAgent {
		return "", fmt.Errorf("%w", model.ErrConfineAgentDisabled)
	}
	if sandboxID == "" {
		return "", fmt.Errorf("confined session: sandbox id must not be empty")
	}
	sessionID, err := s.newID()
	if err != nil {
		return "", fmt.Errorf("confined session: new id: %w", err)
	}
	if err := s.sink.InjectCredentials(ctx, sandboxID, sessionID, creds); err != nil {
		return "", fmt.Errorf("confined session: inject credentials: %w", err)
	}
	detail, err := credentialAuditDetail(sessionID, creds)
	if err != nil {
		return "", fmt.Errorf("confined session: build audit detail: %w", err)
	}
	ev := &model.SandboxEvent{
		SandboxID: sandboxID, TaskID: taskID,
		EventType: model.EventCredentialsInjected, Detail: detail,
		CreatedAt: s.clock(),
	}
	if err := s.events.AppendSandboxEvent(ctx, ev); err != nil {
		return "", fmt.Errorf("confined session: audit: %w", err)
	}
	return sessionID, nil
}

// credentialAuditDetail renders the credentials_injected event Detail as
// JSON carrying the session ID and credential NAMES only — never the secret
// values (model.RedactedCredentialNames). Refs: MGIT-11.11.4
func credentialAuditDetail(sessionID string, creds []model.SessionCredential) (string, error) {
	payload := struct {
		SessionID       string   `json:"session_id"`
		CredentialNames []string `json:"credential_names"`
	}{SessionID: sessionID, CredentialNames: model.RedactedCredentialNames(creds)}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
