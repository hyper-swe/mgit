// Package policy persists the host-only sandbox enforcement
// configuration (SEC-02): network policy, require_sandbox, image lock
// reference, sensitive-path list, and resource caps. The store root
// lives OUTSIDE every repo and worktree (e.g. ~/.mgit/host/<repo-id>/),
// is never a committable file, and is never mounted into guests. A
// repo may ship suggested defaults; they take effect only through
// explicit host-side adoption. Refs: FR-17.13, FR-17.6, MGIT-11.3.4
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
)

// policyFileName is the single policy document under the host root.
const policyFileName = "policy.json"

// EventRecorder receives one record per effective-policy change. The
// service layer wires this to the append-only audit trail; tests may
// substitute a fake. Refs: FR-17.13
type EventRecorder interface {
	RecordPolicyChange(ctx context.Context, detail string) error
}

// Store reads and writes the host-only sandbox policy.
// Refs: FR-17.13
type Store struct {
	hostRoot string
	clock    func() time.Time
	events   EventRecorder
}

// NewStore creates a policy store rooted at hostRoot (the per-repo
// host config directory, never inside a worktree).
func NewStore(hostRoot string, clock func() time.Time, events EventRecorder) (*Store, error) {
	if hostRoot == "" {
		return nil, fmt.Errorf("policy store: host root must not be empty")
	}
	if clock == nil {
		return nil, fmt.Errorf("policy store: clock must not be nil")
	}
	if events == nil {
		return nil, fmt.Errorf("policy store: event recorder must not be nil")
	}
	return &Store{hostRoot: hostRoot, clock: clock, events: events}, nil
}

// Load returns the effective policy. With no policy file present, the
// safe defaults apply (require_sandbox=true, allowlist networking). A
// present file is unmarshalled OVER the defaults, so omitted fields
// keep their safe values; a corrupt file fails closed rather than
// silently falling back. Worktree files are never consulted.
// Refs: FR-17.6, FR-17.13
func (s *Store) Load(_ context.Context) (model.SandboxPolicy, error) {
	effective := model.DefaultSandboxPolicy()

	data, err := os.ReadFile(s.policyPath())
	if os.IsNotExist(err) {
		return effective, nil
	}
	if err != nil {
		return model.SandboxPolicy{}, fmt.Errorf("read host policy: %w", err)
	}

	if err := json.Unmarshal(data, &effective); err != nil {
		return model.SandboxPolicy{}, fmt.Errorf("parse host policy (failing closed): %w", err)
	}
	if err := effective.Validate(); err != nil {
		return model.SandboxPolicy{}, fmt.Errorf("invalid host policy (failing closed): %w", err)
	}
	return effective, nil
}

// Save validates and persists the policy under the host root (0700
// dir, 0600 file) and records the change in the audit trail.
// Refs: FR-17.13
func (s *Store) Save(ctx context.Context, p model.SandboxPolicy) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("save host policy: %w", err)
	}
	if err := os.MkdirAll(s.hostRoot, 0o700); err != nil {
		return fmt.Errorf("create host config root: %w", err)
	}

	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode host policy: %w", err)
	}
	if err := os.WriteFile(s.policyPath(), data, 0o600); err != nil {
		return fmt.Errorf("write host policy: %w", err)
	}

	detail := fmt.Sprintf(`{"changed_at":%q,"policy":%s}`,
		s.clock().UTC().Format(time.RFC3339), data)
	if err := s.events.RecordPolicyChange(ctx, detail); err != nil {
		return fmt.Errorf("record policy change: %w", err)
	}
	return nil
}

// AdoptSuggested explicitly adopts a repo-suggested policy file into
// the host root. This is the ONLY path by which worktree content can
// influence enforcement (SEC-02): the suggestion is validated, copied
// host-side, and the adoption audited. Refs: FR-17.13
func (s *Store) AdoptSuggested(ctx context.Context, suggestedPath string) error {
	data, err := os.ReadFile(filepath.Clean(suggestedPath))
	if err != nil {
		return fmt.Errorf("read suggested policy: %w", err)
	}

	suggested := model.DefaultSandboxPolicy()
	if err := json.Unmarshal(data, &suggested); err != nil {
		return fmt.Errorf("parse suggested policy: %w", err)
	}
	if err := suggested.Validate(); err != nil {
		return fmt.Errorf("reject suggested policy: %w", err)
	}
	return s.Save(ctx, suggested)
}

// policyPath returns the policy document path under the host root.
func (s *Store) policyPath() string {
	return filepath.Join(s.hostRoot, policyFileName)
}
