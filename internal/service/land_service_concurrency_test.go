package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd/land"
)

// blockingResolver maps every task to a configurable sandbox ID so a test can
// drive several DIFFERENT sandboxes (distinct buffers) or one sandbox (the
// single-flight case) through the same service.
type blockingResolver struct{ sandboxID string }

func (r *blockingResolver) Status(_ context.Context, taskID string) (*model.SandboxInfo, error) {
	id := r.sandboxID
	if id == "" {
		id = "sbx-" + taskID // distinct sandbox per task
	}
	return &model.SandboxInfo{ID: id, TaskID: taskID}, nil
}

// gatePuller blocks inside Pull on a release channel and records the peak
// number of CONCURRENT in-flight pulls (each pull holds one buffered pool), so
// a test can prove the buffered-memory cap is never exceeded. It also counts
// total pulls (single-flight coalescing collapses N callers to one pull).
type gatePuller struct {
	pool    []land.Object
	entered chan struct{} // signaled once per Pull entry
	release chan struct{} // closed/fed to let a blocked Pull return
	pulls   int64         // total Pull invocations
	inFlt   int64         // current concurrent pulls
	peak    int64         // max observed concurrent pulls
	discard int64         // Discard invocations
}

func (g *gatePuller) Pull(ctx context.Context, _ string) ([]land.Object, error) {
	atomic.AddInt64(&g.pulls, 1)
	cur := atomic.AddInt64(&g.inFlt, 1)
	for {
		p := atomic.LoadInt64(&g.peak)
		if cur <= p || atomic.CompareAndSwapInt64(&g.peak, p, cur) {
			break
		}
	}
	defer atomic.AddInt64(&g.inFlt, -1)
	if g.entered != nil {
		g.entered <- struct{}{}
	}
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return g.pool, nil
}

func (g *gatePuller) Discard(string) { atomic.AddInt64(&g.discard, 1) }

// safeParents is a concurrency-safe poolParentResolver: the shared fakeParents
// uses plain counters that race when distinct-sandbox lands run in parallel,
// which is a test-fake artifact, not a production concern. This one is stateless.
type safeParents struct{}

func (safeParents) ParentFileSet(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (safeParents) HostHasCommit(context.Context, string) (bool, error) { return false, nil }
func (safeParents) Register([]land.Object) ([]string, error)            { return []string{"id"}, nil }
func (safeParents) Deregister([]string)                                 {}

// safeAttestor is a stateless, concurrency-safe Attestor.
type safeAttestor struct{}

func (safeAttestor) Attest(_ context.Context, sandboxID, commitHash, contentHash string) (*model.Attestation, error) {
	return &model.Attestation{
		SandboxID: sandboxID, CommitHash: commitHash, ContentHash: contentHash,
		Alg: model.AlgEd25519, KeyID: "host-key", HostSignature: []byte("sig"),
		IssuedAt: time.Unix(0, 0).UTC(),
	}, nil
}

// safeOrchestrator is a stateless, concurrency-safe landOrchestrator.
type safeOrchestrator struct{}

func (safeOrchestrator) Land(context.Context, LandRequest) error { return nil }

// concFakes builds a LandService backed by the gate puller and concurrency-safe
// downstream fakes, so the race detector flags only genuine production races.
func concFakes(t *testing.T, resolver SandboxResolver, puller LandPuller, opts ...LandOption) *LandService {
	t.Helper()
	svc, err := NewLandService(resolver, puller, &fakeLedger{}, safeParents{},
		safeAttestor{}, safeOrchestrator{}, fakePolicy{p: policyOff()}, opts...)
	require.NoError(t, err)
	return svc
}

// TestLandService_ConcurrencyCap_BoundsBufferedMemory proves the semaphore
// caps how many pools are buffered AT ONCE: with cap=2 and three lands for
// three distinct sandboxes, never more than two pulls are in flight, so the
// worst-case buffered RAM is bounded to cap × per-pool. Refs: MGIT-11.13.5
func TestLandService_ConcurrencyCap_BoundsBufferedMemory(t *testing.T) {
	b := newPoolBuilder(t)
	pool, _ := singleCommitPool(b, "feat: x", "a.txt", "x")
	g := &gatePuller{pool: pool, entered: make(chan struct{}, 8), release: make(chan struct{})}
	const cap = 2
	svc := concFakes(t, &blockingResolver{}, g,
		WithLandLimits(LandLimits{MaxConcurrentLands: cap, MaxPoolBytes: 4 << 30}))

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = svc.Land(context.Background(), "MGIT-"+string(rune('1'+n)))
		}(i)
	}
	// Wait until exactly cap pulls have entered and are blocked; the third must
	// be parked on the semaphore, not buffering a pool.
	<-g.entered
	<-g.entered
	require.Eventually(t, func() bool { return atomic.LoadInt64(&g.inFlt) == cap },
		time.Second, time.Millisecond, "exactly cap pulls should be in flight")
	// Hold long enough to be confident the third is queued, not buffering.
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(cap), atomic.LoadInt64(&g.inFlt), "third land must not buffer a pool")

	close(g.release)
	wg.Wait()
	assert.LessOrEqual(t, atomic.LoadInt64(&g.peak), int64(cap),
		"buffered pools must never exceed the concurrency cap")
}

// TestLandService_ConcurrencyCap_RejectsBeyondLimit proves a land that cannot
// acquire a slot before its deadline is REJECTED (not silently over-allocating
// a third buffer). Refs: MGIT-11.13.5
func TestLandService_ConcurrencyCap_RejectsBeyondLimit(t *testing.T) {
	b := newPoolBuilder(t)
	pool, _ := singleCommitPool(b, "feat: x", "a.txt", "x")
	g := &gatePuller{pool: pool, entered: make(chan struct{}, 4), release: make(chan struct{})}
	svc := concFakes(t, &blockingResolver{}, g,
		WithLandLimits(LandLimits{MaxConcurrentLands: 1, MaxPoolBytes: 1 << 20}))

	// First land takes the only slot and blocks inside Pull.
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		_, _ = svc.Land(context.Background(), "MGIT-1")
	}()
	<-g.entered // holder is now inside Pull, holding the slot

	// Second land for a DIFFERENT sandbox with a short deadline cannot get the
	// slot and must be rejected rather than buffering a second pool.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := svc.Land(ctx, "MGIT-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "concurrency cap")
	assert.Equal(t, int64(1), atomic.LoadInt64(&g.pulls), "the rejected land must not pull")

	close(g.release)
	<-holderDone
}

// TestLandService_SingleFlight_CoalescesConcurrentTriggers is the F7 regression
// (MGIT-11.10.11): N concurrent triggers for ONE sandbox must coalesce into a
// SINGLE in-flight pull (one pools[sandboxID] slot), not N racing full pulls.
// Refs: MGIT-11.10.11, MGIT-11.13.5
func TestLandService_SingleFlight_CoalescesConcurrentTriggers(t *testing.T) {
	b := newPoolBuilder(t)
	pool, _ := singleCommitPool(b, "feat: x", "a.txt", "x")
	g := &gatePuller{pool: pool, entered: make(chan struct{}, 1), release: make(chan struct{})}
	// One fixed sandbox ID for every task: all calls address the same sandbox.
	svc := concFakes(t, &blockingResolver{sandboxID: "sbx-shared"}, g,
		WithLandLimits(LandLimits{MaxConcurrentLands: 8, MaxPoolBytes: 1 << 20}))

	const n = 6
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sum, err := svc.Land(context.Background(), "MGIT-1")
			if err == nil && sum != nil {
				results[idx] = sum.Commits
			}
		}(i)
	}
	// Wait until the single leader pull is in flight, then let it complete.
	<-g.entered
	// Give the other callers time to coalesce onto the in-flight pull.
	require.Eventually(t, func() bool { return atomic.LoadInt64(&g.inFlt) == 1 },
		time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(1), atomic.LoadInt64(&g.inFlt),
		"only one pull may be in flight for one sandbox")
	close(g.release)
	wg.Wait()

	assert.Equal(t, int64(1), atomic.LoadInt64(&g.pulls),
		"N concurrent triggers for one sandbox must coalesce into ONE pull")
	for i := 0; i < n; i++ {
		assert.Equal(t, 1, results[i], "every coalesced caller shares the leader's result")
	}
}

// TestNewLandService_DefaultLimits_AppliedAndLogged verifies zero/invalid
// bounds fall back to the safe defaults and the effective host budget is
// logged at construction (no silent budget). Refs: MGIT-11.13.5
func TestNewLandService_DefaultLimits_AppliedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	svc := concFakes(t, &blockingResolver{}, &gatePuller{release: make(chan struct{})},
		WithLandLimits(LandLimits{MaxConcurrentLands: 0, MaxPoolBytes: 0}),
		WithLandLogger(logger))

	assert.Equal(t, defaultMaxConcurrentLands, svc.limits.MaxConcurrentLands)
	assert.Equal(t, int64(defaultMaxPoolBytes), svc.limits.MaxPoolBytes)
	assert.Equal(t, defaultMaxConcurrentLands, cap(svc.sem), "semaphore sized to the cap")

	// The budget line records cap × per-pool so an operator can audit it.
	require.True(t, strings.Contains(buf.String(), "land_budget"), "budget event must be logged")
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(firstLine(buf.String())), &rec))
	assert.EqualValues(t, defaultMaxConcurrentLands, rec["max_concurrent_lands"])
	assert.EqualValues(t, int64(defaultMaxConcurrentLands)*defaultMaxPoolBytes, rec["host_memory_budget_bytes"])
}

// firstLine returns the first newline-delimited line of s.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
