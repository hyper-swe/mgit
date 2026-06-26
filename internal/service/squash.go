package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// SquashRequest holds parameters for squashing task commits.
// Refs: FR-7
type SquashRequest struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// SquashService consolidates micro-commits for a task into a single commit.
// Refs: FR-7, MGIT-3.1.2
type SquashService struct {
	commitStore *gitstore.CommitStore
	indexStore  *index.Store
	repo        *gitstore.Repository
	audit       *AuditService
}

// NewSquashService creates a SquashService with injected dependencies.
func NewSquashService(repo *gitstore.Repository, cs *gitstore.CommitStore, idx *index.Store) *SquashService {
	return &SquashService{
		commitStore: cs,
		indexStore:  idx,
		repo:        repo,
	}
}

// WithAudit attaches an AuditService so successful squashes are recorded in the
// append-only audit trail surfaced by `mgit audit`. Returns the receiver for
// fluent wiring. If unset, squashes proceed without an audit entry.
// Refs: FR-12, MGIT-20
func (s *SquashService) WithAudit(a *AuditService) *SquashService {
	s.audit = a
	return s
}

// SquashTask consolidates a task's micro-commits into a single squash commit
// capturing only that task's net changes, placed on a dedicated task/<ID>
// branch parented off the task's base. It does NOT advance the integration
// branch and does NOT remove the originals: per the append-only law (FR-12)
// the micro-commits remain the audit trail and the squash is the task's clean,
// exportable deliverable (consumable via ExportToGitPatch / `git am`). The
// squash is indexed and audited like any commit. Refs: FR-7, FR-12, MGIT-22
func (s *SquashService) SquashTask(ctx context.Context, req SquashRequest) (*model.Commit, error) {
	// Retrieve all commits for this task
	records, err := s.indexStore.GetTaskCommits(ctx, req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("squash task %s: get commits: %w", req.TaskID, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("squash task %s: %w", req.TaskID, model.ErrTaskNotFound)
	}

	// Merge all file diffs from individual commits. base is the task's base
	// (the first micro-commit's parent) — the "from" side of the net export diff.
	var allDiffs []model.FileDiff
	var commitSummaries []string
	var base string
	for i, rec := range records {
		c, getErr := s.commitStore.GetCommit(ctx, rec.CommitHash)
		if getErr != nil {
			return nil, fmt.Errorf("squash task %s: get commit %s: %w", req.TaskID, rec.CommitHash, getErr)
		}
		if i == 0 {
			base = c.ParentID
		}
		allDiffs = append(allDiffs, c.FileDiffs...)
		commitSummaries = append(commitSummaries,
			fmt.Sprintf("- %s: %s", c.ShortID(), c.Message))
	}

	// ADR-008 §4: a task's net change is computed against its PINNED fork-base.
	// Enforce that the computed base still matches the pin — fail loud if a base
	// move or retarget ever shifted it, rather than export a corrupt patch.
	if err := assertPinnedForkBase(ctx, s.indexStore, req.TaskID, base); err != nil {
		return nil, fmt.Errorf("squash task %s: %w", req.TaskID, err)
	}

	// Merge diffs: last write wins per path
	mergedDiffs := mergeDiffs(allDiffs)

	// Build squash message
	message := req.Message
	if message == "" {
		message = fmt.Sprintf("[%s] Squashed from %d micro-commits", req.TaskID, len(records))
	}
	if len(commitSummaries) > 0 {
		message = message + "\n\n" + strings.Join(commitSummaries, "\n")
	}

	if req.DryRun {
		// Return what would be created without making changes
		taskID, parseErr := model.ParseTaskID(req.TaskID)
		if parseErr != nil {
			return nil, fmt.Errorf("squash task: %w", parseErr)
		}
		return &model.Commit{
			TaskID:     taskID,
			Message:    message,
			FileDiffs:  mergedDiffs,
			CommitType: model.CommitTypeSquash,
		}, nil
	}

	// Create the squash commit
	taskID, err := model.ParseTaskID(req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("squash task: %w", err)
	}

	squashCommit := &model.Commit{
		TaskID:     taskID,
		AgentID:    "mgit-squash",
		Message:    message,
		FileDiffs:  mergedDiffs,
		CommitType: model.CommitTypeSquash,
		CreatedBy:  "mgit-squash",
		Branch:     "task/" + req.TaskID,
	}

	// The squash captures only this task's net changes on a dedicated task
	// branch, parented off the task's base — it never advances the integration
	// branch and never removes the originals (append-only, FR-12). MGIT-22.
	taskHashes := make([]string, len(records))
	for i, rec := range records {
		taskHashes[i] = rec.CommitHash
	}
	hash, err := s.commitStore.CreateSquashCommit(ctx, gitstore.SquashCommitParams{
		Commit:      squashCommit,
		TaskCommits: taskHashes,
		Branch:      squashCommit.Branch,
	})
	if err != nil {
		return nil, fmt.Errorf("squash task %s: create squash commit: %w", req.TaskID, err)
	}
	// Bind the created commit's identity so GitFormatPatch can compute the net
	// base->squash tree diff for the git-am-compatible export. Refs: MGIT-33
	squashCommit.CommitID = hash
	squashCommit.ParentID = base

	// Index the squash commit
	position := len(records)
	err = s.indexStore.AddCommitToTask(ctx, req.TaskID, hash, squashCommit.ContentHash, "mgit-squash", position)
	if err != nil {
		return nil, fmt.Errorf("squash task %s: index squash commit: %w", req.TaskID, err)
	}

	// Record the operation in the append-only audit trail (MGIT-20).
	if err := s.logAudit(squashCommit, req.TaskID, len(records)); err != nil {
		return nil, fmt.Errorf("squash task %s: audit: %w", req.TaskID, err)
	}

	return squashCommit, nil
}

// logAudit appends a SQUASH entry to the audit trail. No-op when no
// AuditService is wired. Refs: FR-12, MGIT-20
func (s *SquashService) logAudit(c *model.Commit, taskID string, n int) error {
	if s.audit == nil {
		return nil
	}
	return s.audit.LogOperation(AuditEntry{
		Operation: AuditSquash,
		AgentID:   c.AgentID,
		TaskID:    taskID,
		CommitID:  c.CommitID,
		Details:   fmt.Sprintf("squashed %d commits", n),
	})
}

// ExportToGitPatch renders a squash commit as a standard git format-patch
// (mbox) text. The first line of the message is prefixed with "[squashed]"
// per FR-7 / MGIT-4.2.2 so downstream git tooling can recognize the patch
// as originating from an mgit squash operation. The output is consumable by
// "git am" in any standard git repository.
// Refs: FR-7, MGIT-4.2.2
func (s *SquashService) ExportToGitPatch(c *model.Commit) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(s.mboxHeader(c))

	// Per-file diff section. Each file gets a "diff --git" header so the
	// output is recognizable to git am / git apply.
	for _, d := range c.FileDiffs {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", d.Path, d.Path)
		switch d.Operation {
		case model.DiffAdded:
			fmt.Fprintf(&b, "new file mode 100644\n")
		case model.DiffDeleted:
			fmt.Fprintf(&b, "deleted file mode 100644\n")
		}
		if d.OldHash != "" || d.NewHash != "" {
			fmt.Fprintf(&b, "index %s..%s\n", shortPatchHash(d.OldHash), shortPatchHash(d.NewHash))
		}
		fmt.Fprintf(&b, "--- a/%s\n", d.Path)
		fmt.Fprintf(&b, "+++ b/%s\n", d.Path)
		for _, h := range d.Hunks {
			fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n",
				h.LineStart, h.LinesRemoved, h.LineStart, h.LinesAdded)
			if h.Content != "" {
				b.WriteString(h.Content)
				if !strings.HasSuffix(h.Content, "\n") {
					b.WriteString("\n")
				}
			}
		}
	}

	b.WriteString("-- \nmgit\n")
	return b.String()
}

// mboxHeader renders the git format-patch mbox preamble (From/From:/Date/
// Subject + optional body + the "---" separator) shared by ExportToGitPatch and
// GitFormatPatch. The subject is prefixed with [squashed] per FR-7. Refs: FR-7
func (s *SquashService) mboxHeader(c *model.Commit) string {
	subject, body := splitMessage(c.Message)
	if !strings.HasPrefix(subject, "[squashed]") {
		subject = "[squashed] " + subject
	}
	author := c.AgentID
	if author == "" {
		author = "mgit-squash"
	}
	createdAt := c.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	hash := c.CommitID
	if hash == "" {
		hash = c.ContentHash
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From %s %s\n", hash, createdAt.UTC().Format("Mon Jan 2 15:04:05 2006"))
	fmt.Fprintf(&b, "From: %s <%s@mgit.local>\n", author, author)
	fmt.Fprintf(&b, "Date: %s\n", createdAt.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Subject: [PATCH] %s\n\n", subject)
	if body != "" {
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	return b.String()
}

// GitFormatPatch renders a squash commit as a git format-patch whose body is
// produced by go-git's own unified-diff encoder (DiffStore.PatchBetween) — so
// it is git-am/git-apply compatible with real content, the working mgit->git
// delivery bridge. Unlike ExportToGitPatch (which renders model.FileDiff
// metadata, for synthetic/unit commits), this computes the real net tree diff
// from the squash commit's parent (the task base) to the squash commit, so
// c.CommitID must be set (SquashTask sets it). Refs: FR-7, MGIT-33
func (s *SquashService) GitFormatPatch(ctx context.Context, c *model.Commit) (string, error) {
	if c == nil {
		return "", nil
	}
	body, err := gitstore.NewDiffStore(s.repo).PatchBetween(ctx, c.ParentID, c.CommitID)
	if err != nil {
		return "", fmt.Errorf("squash git patch: %w", err)
	}
	return s.mboxHeader(c) + body + "-- \nmgit\n", nil
}

// splitMessage splits a commit message into its first-line subject and body.
func splitMessage(msg string) (subject, body string) {
	idx := strings.Index(msg, "\n")
	if idx < 0 {
		return strings.TrimSpace(msg), ""
	}
	return strings.TrimSpace(msg[:idx]), strings.TrimLeft(msg[idx+1:], "\n")
}

// shortPatchHash returns an 8-char prefix or "00000000" if empty.
func shortPatchHash(h string) string {
	if h == "" {
		return "00000000"
	}
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}

// mergeDiffs consolidates file diffs, keeping the last operation per path.
func mergeDiffs(diffs []model.FileDiff) []model.FileDiff {
	seen := make(map[string]model.FileDiff)
	var order []string

	for _, d := range diffs {
		if _, exists := seen[d.Path]; !exists {
			order = append(order, d.Path)
		}
		seen[d.Path] = d
	}

	merged := make([]model.FileDiff, 0, len(order))
	for _, path := range order {
		merged = append(merged, seen[path])
	}
	return merged
}
