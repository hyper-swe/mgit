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
}

// NewSquashService creates a SquashService with injected dependencies.
func NewSquashService(repo *gitstore.Repository, cs *gitstore.CommitStore, idx *index.Store) *SquashService {
	return &SquashService{
		commitStore: cs,
		indexStore:  idx,
		repo:        repo,
	}
}

// SquashTask consolidates all commits for a task into a single squash commit.
// Original commits remain in history (append-only per FR-12).
// Refs: FR-7
func (s *SquashService) SquashTask(ctx context.Context, req SquashRequest) (*model.Commit, error) {
	// Retrieve all commits for this task
	records, err := s.indexStore.GetTaskCommits(ctx, req.TaskID)
	if err != nil {
		return nil, fmt.Errorf("squash task %s: get commits: %w", req.TaskID, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("squash task %s: %w", req.TaskID, model.ErrTaskNotFound)
	}

	// Merge all file diffs from individual commits
	var allDiffs []model.FileDiff
	var commitSummaries []string
	for _, rec := range records {
		c, getErr := s.commitStore.GetCommit(ctx, rec.CommitHash)
		if getErr != nil {
			return nil, fmt.Errorf("squash task %s: get commit %s: %w", req.TaskID, rec.CommitHash, getErr)
		}
		allDiffs = append(allDiffs, c.FileDiffs...)
		commitSummaries = append(commitSummaries,
			fmt.Sprintf("- %s: %s", c.ShortID(), c.Message))
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

	hash, err := s.commitStore.CreateCommit(ctx, squashCommit)
	if err != nil {
		return nil, fmt.Errorf("squash task %s: create squash commit: %w", req.TaskID, err)
	}

	// Index the squash commit
	position := len(records)
	err = s.indexStore.AddCommitToTask(ctx, req.TaskID, hash, squashCommit.ContentHash, "mgit-squash", position)
	if err != nil {
		return nil, fmt.Errorf("squash task %s: index squash commit: %w", req.TaskID, err)
	}

	return squashCommit, nil
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

	// Split message into subject + body, prefix subject with [squashed].
	subject, body := splitMessage(c.Message)
	if !strings.HasPrefix(subject, "[squashed]") {
		subject = "[squashed] " + subject
	}

	author := c.AgentID
	if author == "" {
		author = "mgit-squash"
	}
	email := author + "@mgit.local"
	createdAt := c.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	dateRFC := createdAt.UTC().Format(time.RFC1123Z)
	hash := c.CommitID
	if hash == "" {
		hash = c.ContentHash
	}

	var b strings.Builder
	// mbox From line — git format-patch uses commit hash + epoch.
	fmt.Fprintf(&b, "From %s %s\n", hash, createdAt.UTC().Format("Mon Jan 2 15:04:05 2006"))
	fmt.Fprintf(&b, "From: %s <%s>\n", author, email)
	fmt.Fprintf(&b, "Date: %s\n", dateRFC)
	fmt.Fprintf(&b, "Subject: [PATCH] %s\n", subject)
	b.WriteString("\n")
	if body != "" {
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("---\n")

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
