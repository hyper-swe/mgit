package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hyper-swe/mgit/internal/model"
)

// relayedBaseShapeRefusal is the launch refusal as it reaches the CLI through
// a daemon result frame: text only, the sentinel's identity gone.
const relayedBaseShapeRefusal = "sandbox exec: sandbox ensure-running: kvm launch: " +
	"guest base is a shape this backend cannot boot: the kvm (firecracker) backend boots a kernel + ext4 " +
	"rootfs image, but guest base base@sha256:4a64d65e is a directory (/home/u/.cache/mgit/bases/4a64); " +
	"install an image it can boot with `mgit sandbox image install --from <dir-or-url>`, or run the Linux " +
	"release archive's daemon, which links libkrun and boots a directory base"

// The released v0.6.8 said "mgit could not identify what failed here" under
// an error whose cause was fully determined: the linked backend cannot boot
// the registered base's shape. The daemon now refuses that at launch, with the
// cause and the fix in its words, and the CLI recognises the refusal instead
// of disowning it. A v0.6.8 daemon's own words for the same cause (the VMM's
// `failed to stat kernel image path, ""`: the base names no kernel at all)
// are recognised too, since that daemon is still installed. Refs: MGIT-233
func TestClassifyGuestFailure_AnUnbootableBaseIsNamed(t *testing.T) {
	for name, err := range map[string]error{
		"in_process_sentinel": fmt.Errorf("kvm launch: %w: the kvm (firecracker) backend boots …", model.ErrGuestBaseUnbootable),
		"relayed_refusal":     errors.New(relayedBaseShapeRefusal),
		"a_v068_daemon":       errors.New(`sandbox exec: sandbox ensure-running: kvm launch: start vm: firecracker config invalid: failed to stat kernel image path, ""`),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, phaseBaseUnbootable, classifyGuestFailure(err, entitlementUnknown).phase)
		})
	}
	// A kernel path that names a file is a different fault (it vanished), and
	// is not claimed as a shape the backend cannot boot.
	other := errors.New(`kvm launch: start vm: firecracker config invalid: failed to stat kernel image path, "/img/vmlinux"`)
	assert.NotEqual(t, phaseBaseUnbootable, classifyGuestFailure(other, entitlementUnknown).phase)
}

func TestWriteGuestFailure_UnbootableBase_NamesItAndDisownsNothing(t *testing.T) {
	var out bytes.Buffer
	writeGuestFailure(&out, advisoryInfo(), classifyGuestFailure(errors.New(relayedBaseShapeRefusal), entitlementUnknown))
	got := out.String()

	assert.NotContains(t, got, "could not identify", "the cause is determined, so mgit does not disown it")
	assertNoInGuestMemoryClaim(t, got)
	assert.Contains(t, got, "no VM was started", "the reader is told what did NOT happen")
	assert.Contains(t, got, "base/boots", "doctor's row says the same, with both sides named")
}
