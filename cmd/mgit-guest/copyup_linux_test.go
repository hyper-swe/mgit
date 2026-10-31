//go:build linux

package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

// The guest recognizes the copy-up refusal by EOPNOTSUPP and nothing else. The
// wiring in mountWorktree is only exercised by a real libkrun VM, so this pins
// the predicate it uses. Refs: MGIT-230.8, MGIT-230.7
func TestCopyUpRefusal_IsExactlyEOPNOTSUPP(t *testing.T) {
	assert.True(t, copyUpRefusal(unix.EOPNOTSUPP))
	assert.True(t, copyUpRefusal(fmt.Errorf("mkdir /home/a: %w", unix.EOPNOTSUPP)), "a wrapped refusal is still the refusal")
	for _, err := range []error{nil, unix.ENOENT, unix.EACCES, unix.EEXIST, unix.ENOSPC, fmt.Errorf("other")} {
		assert.False(t, copyUpRefusal(err), "%v is not the copy-up refusal", err)
	}
}
