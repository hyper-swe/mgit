package service

import (
	"context"
	"fmt"

	gitstore "github.com/hyper-swe/mgit-dev/internal/store/git"
)

// DefaultGCPackThreshold is the loose-object count above which a normal
// (non-aggressive) gc will pack objects into a packfile.
// Refs: FR-8.4, FR-13.2, MGIT-4.2.11
const DefaultGCPackThreshold = 1000

// GCRequest holds the parameters for a garbage-collection run.
// Refs: MGIT-4.2.11
type GCRequest struct {
	Aggressive    bool
	PackThreshold int
	AutoTriggered bool
}

// GCResult reports the outcome of a gc run, suitable for JSON output.
// Refs: MGIT-4.2.11
type GCResult struct {
	LooseBefore   int    `json:"loose_objects_before"`
	LooseAfter    int    `json:"loose_objects_after"`
	BytesBefore   int64  `json:"bytes_before"`
	BytesAfter    int64  `json:"bytes_after"`
	BytesSaved    int64  `json:"bytes_saved"`
	Packed        bool   `json:"packed"`
	Aggressive    bool   `json:"aggressive"`
	AutoTriggered bool   `json:"auto_triggered"`
	Status        string `json:"status"`
}

// GCService runs garbage collection on the underlying object store.
// Refs: FR-8.4, FR-13.2, MGIT-4.2.11
type GCService struct {
	store *gitstore.GCStore
}

// NewGCService creates a GCService backed by the given GCStore.
func NewGCService(store *gitstore.GCStore) *GCService {
	return &GCService{store: store}
}

// Run executes a garbage collection pass.
//
// Behavior:
//   - If req.Aggressive is true, loose objects are packed unconditionally.
//   - Otherwise, packing happens only when the loose object count exceeds
//     req.PackThreshold (default DefaultGCPackThreshold).
//   - If neither condition is met, the operation is a no-op and the
//     status is "no-op".
//
// Refs: FR-8.4
func (s *GCService) Run(ctx context.Context, req GCRequest) (*GCResult, error) {
	threshold := req.PackThreshold
	if threshold <= 0 {
		threshold = DefaultGCPackThreshold
	}

	looseBefore, err := s.store.LooseObjectCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: count loose: %w", err)
	}
	bytesBefore, err := s.store.ObjectsDirSize(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: size before: %w", err)
	}

	result := &GCResult{
		LooseBefore:   looseBefore,
		BytesBefore:   bytesBefore,
		Aggressive:    req.Aggressive,
		AutoTriggered: req.AutoTriggered,
	}

	shouldPack := req.Aggressive || looseBefore > threshold
	if !shouldPack {
		result.LooseAfter = looseBefore
		result.BytesAfter = bytesBefore
		result.Status = "no-op"
		return result, nil
	}

	if err := s.store.PackLooseObjects(ctx, req.Aggressive); err != nil {
		return nil, fmt.Errorf("gc: pack: %w", err)
	}

	looseAfter, err := s.store.LooseObjectCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: count loose after: %w", err)
	}
	bytesAfter, err := s.store.ObjectsDirSize(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: size after: %w", err)
	}

	result.LooseAfter = looseAfter
	result.BytesAfter = bytesAfter
	result.BytesSaved = bytesBefore - bytesAfter
	result.Packed = true
	result.Status = "packed"
	if result.AutoTriggered {
		result.Status = "packed (auto)"
	}
	return result, nil
}
