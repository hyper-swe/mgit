package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

// The first boot still refuses a worktree that holds the store, as defence
// in depth behind registration's refusal. Its error names the cause exactly,
// so the footer saying mgit could not identify what failed is wrong for it,
// and so is its "do not resize the sandbox" advice: no VM was started and the
// fix is the worktree. Matched by text too, because the error reaches the CLI
// as a string. Refs: MGIT-222, MGIT-118
func TestGuestFailure_ASharedStoreRefusal_IsPlacedNotUnidentified(t *testing.T) {
	boot := errors.New(`libkrun launch: bind private store: shared object store reachable from the guest: ` +
		`shared store "/p/repo/.mgit" is inside the mounted worktree "/p/repo"`)
	for name, err := range map[string]error{"as_text": boot, "as_the_sentinel": model.ErrSharedStoreReachable} {
		t.Run(name, func(t *testing.T) {
			f := classifyGuestFailure(err, entitlementUnknown)
			assert.Equal(t, phaseLayoutRefused, f.phase)
			var out bytes.Buffer
			writeGuestFailure(&out, &model.SandboxInfo{TaskID: "MGIT-222"}, f)
			assert.NotContains(t, out.String(), "could not identify")
			assert.NotContains(t, out.String(), "resize")
			for _, want := range []string{"no VM was started", "mgit work", "mgit worktree add", "outside this repository"} {
				assert.Contains(t, out.String(), want)
			}
		})
	}
}
