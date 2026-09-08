package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hyper-swe/mgit/internal/model"
)

// SetExecIdentity wires the identity guest execs run as: the daemon's own
// uid/gid, because that is the identity it delivered the worktree and the
// composed base as (an unprivileged daemon can chown neither to anyone
// else), plus the name and home the guest materializes for it. Set once at
// daemon wiring time; unset leaves the guest's own default and a verdict
// that says so. Refs: MGIT-151
func (s *SandboxService) SetExecIdentity(id model.GuestIdentity) {
	s.execIdentity = &id
}

// ExecIdentity reports the identity guest execs run as, or nil when none
// is wired. Refs: MGIT-151
func (s *SandboxService) ExecIdentity() *model.GuestIdentity {
	if s.execIdentity == nil {
		return nil
	}
	id := *s.execIdentity
	return &id
}

// decideExecIdentity settles the identity an exec runs as BEFORE it is
// sent: a client-chosen run_as is refused (the only road to root is the
// audited as_root), an escalation is audited and asks the guest for root
// explicitly, and everything else runs as the daemon's identity.
// Refs: MGIT-151, FR-17.18, SEC-05
func (s *SandboxService) decideExecIdentity(
	ctx context.Context, info *model.SandboxInfo, req model.ExecRequest,
) (model.ExecRequest, error) {
	if req.RunAs != nil {
		return req, fmt.Errorf("sandbox exec: %w", &model.ValidationError{
			Field:   "run_as",
			Message: "the identity a command runs as is the daemon's to choose; use --as-root to escalate, which is audited",
		})
	}
	if req.AsRoot {
		root := model.RootIdentity()
		req.RunAs = &root
		if err := s.auditEscalation(ctx, info, req); err != nil {
			return req, err
		}
		return req, nil
	}
	if s.execIdentity != nil {
		id := *s.execIdentity
		req.RunAs = &id
	}
	return req, nil
}

// escalationDetail is what the audit records about a root exec: the
// program and how many arguments it got — never the arguments, which may
// carry secrets. Refs: MGIT-151, FR-17.18
type escalationDetail struct {
	Program string `json:"program"`
	Args    int    `json:"args"`
}

// auditEscalation writes the exec_privileged event; a failed write refuses
// the exec, since an unrecorded escalation is the thing the event exists
// to prevent. Refs: MGIT-151, FR-17.18
func (s *SandboxService) auditEscalation(ctx context.Context, info *model.SandboxInfo, req model.ExecRequest) error {
	detail, err := json.Marshal(escalationDetail{Program: req.Command[0], Args: len(req.Command) - 1})
	if err != nil {
		return fmt.Errorf("sandbox exec: encode escalation audit: %w", err)
	}
	if err := s.events.AppendSandboxEvent(ctx, &model.SandboxEvent{
		SandboxID: info.ID, TaskID: info.TaskID, EventType: model.EventExecPrivileged,
		NetworkMode: info.NetworkMode, Detail: string(detail), CreatedAt: s.clock().UTC(),
	}); err != nil {
		return fmt.Errorf("sandbox exec: audit the escalation: %w", err)
	}
	return nil
}
