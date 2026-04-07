// Package testutil provides test infrastructure for mgit.
// Refs: MGIT-1.2.5, NFR-4
package testutil

import (
	"sync"
	"time"
)

// MockClock provides deterministic time control for testing.
// It is safe for concurrent use.
// Refs: NFR-4 (clock injection requirement)
type MockClock struct {
	mu      sync.Mutex
	current time.Time
}

// NewMockClock creates a MockClock initialized to the given time.
func NewMockClock(t time.Time) *MockClock {
	return &MockClock{current: t}
}

// Now returns the current mock time.
func (c *MockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Advance moves the clock forward by the given duration.
func (c *MockClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
}

// Set updates the clock to the given time.
func (c *MockClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = t
}

// ClockFunc returns a func() time.Time suitable for dependency injection.
func (c *MockClock) ClockFunc() func() time.Time {
	return c.Now
}
