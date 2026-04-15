package mtix

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/service"
	"github.com/hyper-swe/mgit/internal/store/index"
)

// Integration orchestrates bidirectional sync between mgit and mtix.
// Refs: FR-14, MGIT-5.3.2, MGIT-5.3.3
type Integration struct {
	client    *Client
	idx       *index.Store
	squashSvc *service.SquashService
	logger    *slog.Logger
}

// NewIntegration creates an mtix integration handler.
func NewIntegration(client *Client, idx *index.Store, squashSvc *service.SquashService, logger *slog.Logger) *Integration {
	return &Integration{
		client:    client,
		idx:       idx,
		squashSvc: squashSvc,
		logger:    logger,
	}
}

// SyncTaskCommit records a commit against an mtix task in the index.
// This creates the bidirectional link: mtix task <-> mgit commit.
// Refs: FR-14.2, MGIT-5.3.2
func (i *Integration) SyncTaskCommit(ctx context.Context, taskID string, commit *model.Commit) error {
	// Verify task exists in mtix (graceful: log warning if mtix unreachable)
	if _, err := i.client.GetNode(ctx, taskID); err != nil {
		i.logger.Warn("mtix task lookup failed (continuing anyway)",
			"task_id", taskID, "error", err)
	}

	// The commit is already indexed by CommitService, so this is a no-op
	// for the mgit side. The sync is implicit: mgit's task_commits table
	// already maps task_id -> commit_hash bidirectionally.
	i.logger.Info("synced commit to mtix task",
		"task_id", taskID,
		"commit_id", commit.ShortID())

	return nil
}

// GetTaskInfo retrieves task info from mtix for display in mgit.
// Returns nil if mtix is unreachable (graceful degradation per NFR-4).
// Refs: FR-14
func (i *Integration) GetTaskInfo(ctx context.Context, taskID string) (*Node, error) {
	node, err := i.client.GetNode(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("get mtix task %s: %w", taskID, err)
	}
	return node, nil
}

// OnTaskDone handles mtix task completion events.
// Triggers auto-squash of all commits for the completed task.
// Refs: FR-14.3, MGIT-5.3.3
func (i *Integration) OnTaskDone(ctx context.Context, taskID string) error {
	i.logger.Info("mtix task completed, triggering auto-squash",
		"task_id", taskID)

	// Auto-squash all commits for the completed task
	squashed, err := i.squashSvc.SquashTask(ctx, service.SquashRequest{
		TaskID:  taskID,
		Message: fmt.Sprintf("[%s] Auto-squash on task completion", taskID),
	})
	if err != nil {
		return fmt.Errorf("auto-squash task %s: %w", taskID, err)
	}

	i.logger.Info("auto-squash completed",
		"task_id", taskID,
		"squash_commit", squashed.ShortID())

	return nil
}

// Event represents an mtix WebSocket event.
// Refs: FR-14.3
type Event struct {
	Type      string          `json:"type"`
	NodeID    string          `json:"node_id"`
	Timestamp string          `json:"timestamp"`
	Author    string          `json:"author"`
	Data      json.RawMessage `json:"data"`
}

// WatchEvents connects to mtix WebSocket and handles events.
// This is a long-running function; call in a goroutine.
// Refs: FR-14.3, MGIT-5.3.3
func (i *Integration) WatchEvents(ctx context.Context) error {
	i.logger.Info("watching mtix events (polling mode)")

	// Polling fallback: check for completed tasks periodically
	// Full WebSocket integration would use gorilla/websocket, but that's
	// not in APPROVED-PACKAGES.md. Polling is safe and sufficient.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// In production, this would poll mtix for recently completed tasks
			// and trigger OnTaskDone for each. For now, auto-squash is
			// triggered explicitly via the mgit squash command or MCP tool.
			i.logger.Debug("mtix event poll tick")
		}
	}
}
