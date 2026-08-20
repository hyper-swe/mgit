package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// fakeCapturer scripts a sequence of fingerprints and records captures.
type fakeCapturer struct {
	fps        []string
	i          int
	captured   []string // fingerprint at each capture
	pruned     []int
	fpErr      error
	captureErr error
}

func (f *fakeCapturer) Fingerprint() (string, error) {
	if f.fpErr != nil {
		return "", f.fpErr
	}
	if f.i >= len(f.fps) {
		return f.fps[len(f.fps)-1], nil
	}
	fp := f.fps[f.i]
	f.i++
	return fp, nil
}

func (f *fakeCapturer) Capture(_ context.Context, taskID string, at time.Time) (*model.Snapshot, error) {
	if f.captureErr != nil {
		return nil, f.captureErr
	}
	fp := f.fps[minInt(f.i-1, len(f.fps)-1)]
	f.captured = append(f.captured, fp)
	return &model.Snapshot{ID: "S" + taskID, TaskID: taskID, Fingerprint: fp, CapturedAt: at}, nil
}

func (f *fakeCapturer) Prune(_ context.Context, _ string, keep int) (int, error) {
	f.pruned = append(f.pruned, keep)
	return 0, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func fixedClockAt(base time.Time) func() time.Time {
	n := 0
	return func() time.Time { n++; return base.Add(time.Duration(n) * time.Minute) }
}

// QUIESCENCE, stated as behavior. A capture happens when the worktree has
// STOPPED changing — one full interval with an unchanged fingerprint — and not
// while it is mid-edit. Capturing on every tick would record torn, half-written
// states and bury the useful ones. Refs: MGIT-110, R-H234
func TestSnapshotService_Observe_CapturesOnlyWhenTheWorktreeHasSettled(t *testing.T) {
	tests := []struct {
		name         string
		fingerprints []string
		wantCaptures int
		explain      string
	}{
		{
			name:         "still_changing_never_captures",
			fingerprints: []string{"a", "b", "c", "d"},
			wantCaptures: 0,
			explain:      "a worktree edited on every tick is never settled",
		},
		{
			name:         "settles_once_captures_once",
			fingerprints: []string{"a", "b", "b"},
			wantCaptures: 1,
			explain:      "changed, then held still for an interval",
		},
		{
			name:         "stays_settled_does_not_recapture",
			fingerprints: []string{"a", "b", "b", "b", "b"},
			wantCaptures: 1,
			explain:      "an idle agent must not accumulate identical snapshots",
		},
		{
			name:         "changes_again_after_settling_captures_again",
			fingerprints: []string{"a", "b", "b", "c", "c"},
			wantCaptures: 2,
			explain:      "each settled state earns one capture",
		},
		{
			name:         "unchanged_from_the_very_start_captures_once",
			fingerprints: []string{"a", "a"},
			wantCaptures: 1,
			explain:      "an agent that edited before the watcher started is still recoverable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := &fakeCapturer{fps: tt.fingerprints}
			svc := NewSnapshotService(cap, fixedClockAt(time.Unix(1700000000, 0).UTC()), 10)
			for range tt.fingerprints {
				_, err := svc.Observe(context.Background(), "MGIT-110")
				require.NoError(t, err)
			}
			assert.Len(t, cap.captured, tt.wantCaptures, tt.explain)
		})
	}
}

// Retention runs with the capture, so a long run cannot grow snapshots without
// bound. Refs: MGIT-110
func TestSnapshotService_Observe_PrunesToTheRetentionLimitAfterCapturing(t *testing.T) {
	cap := &fakeCapturer{fps: []string{"a", "a"}}
	svc := NewSnapshotService(cap, fixedClockAt(time.Unix(1700000000, 0).UTC()), 7)
	for i := 0; i < 2; i++ {
		_, err := svc.Observe(context.Background(), "MGIT-110")
		require.NoError(t, err)
	}
	require.Len(t, cap.captured, 1)
	assert.Equal(t, []int{7}, cap.pruned, "retention is applied with the capture, not left to grow")
}

// A snapshotter that cannot read the worktree must report it, never pretend a
// capture happened. Refs: MGIT-110
func TestSnapshotService_Observe_SurfacesFailuresRatherThanClaimingACapture(t *testing.T) {
	t.Run("fingerprint_failure", func(t *testing.T) {
		cap := &fakeCapturer{fps: []string{"a"}, fpErr: errors.New("worktree unreadable")}
		svc := NewSnapshotService(cap, fixedClockAt(time.Unix(0, 0)), 5)
		snap, err := svc.Observe(context.Background(), "MGIT-110")
		require.Error(t, err)
		assert.Nil(t, snap)
	})
	t.Run("capture_failure", func(t *testing.T) {
		cap := &fakeCapturer{fps: []string{"a", "a"}, captureErr: errors.New("disk full")}
		svc := NewSnapshotService(cap, fixedClockAt(time.Unix(0, 0)), 5)
		_, err := svc.Observe(context.Background(), "MGIT-110")
		require.NoError(t, err)
		snap, err := svc.Observe(context.Background(), "MGIT-110")
		require.Error(t, err)
		assert.Nil(t, snap)
	})
}

// A failed capture must not mark the state as captured: the next settled tick
// has to try again, or one transient error would silently cost the whole run
// its recovery point. Refs: MGIT-110
func TestSnapshotService_Observe_AFailedCaptureIsRetriedOnTheNextTick(t *testing.T) {
	cap := &fakeCapturer{fps: []string{"a", "a", "a"}, captureErr: errors.New("transient")}
	svc := NewSnapshotService(cap, fixedClockAt(time.Unix(0, 0)), 5)
	_, err := svc.Observe(context.Background(), "MGIT-110")
	require.NoError(t, err)
	_, err = svc.Observe(context.Background(), "MGIT-110")
	require.Error(t, err)

	cap.captureErr = nil
	snap, err := svc.Observe(context.Background(), "MGIT-110")
	require.NoError(t, err)
	require.NotNil(t, snap, "the settled state must still be captured after a transient failure")
}

// Each task's quiescence is tracked separately: one busy task must not suppress
// a quiet one's snapshot. Refs: MGIT-110
func TestSnapshotService_Observe_TracksEachTaskIndependently(t *testing.T) {
	cap := &fakeCapturer{fps: []string{"x", "x"}}
	svc := NewSnapshotService(cap, fixedClockAt(time.Unix(0, 0)), 5)

	_, err := svc.Observe(context.Background(), "TASK-A")
	require.NoError(t, err)
	// TASK-B's first observation must not inherit TASK-A's "seen" state.
	snap, err := svc.Observe(context.Background(), "TASK-B")
	require.NoError(t, err)
	assert.Nil(t, snap, "a task's first observation establishes a baseline, it does not capture")
}
