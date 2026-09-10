package model

import (
	"errors"
	"strings"
)

// ErrGuestDead reports a sandbox whose guest died: it was reached, then
// stopped answering, and nothing runs in it any more. Refs: MGIT-99
var ErrGuestDead = errors.New("sandbox guest is dead")

// lostServingMarkers are the transport failures that only a connection to a
// guest which HAD been reached can produce: a stream that ended mid-frame, a
// peer that reset or closed it, or a re-dial refused by a guest that was
// answering a moment ago.
//
// The list is deliberately short, and what it leaves out is the point. A read
// deadline expiring ("i/o timeout") is NOT here: the host waited and gave up,
// which is a statement about the host's patience and not about the guest —
// yet as the leftover branch it was once reported as in-guest memory
// exhaustion on a sandbox that was perfectly healthy (MGIT-122).
//
// It lives in the model because two layers judge the same failure: the
// daemon marks the sandbox dead on it (MGIT-99) and the CLI explains it
// (MGIT-118); a marker in one list and not the other would have them
// disagree about one event. Refs: MGIT-99, MGIT-118, MGIT-122, MGIT-95
var lostServingMarkers = []string{
	"EOF",
	"connection reset",
	"broken pipe",
	"use of closed network connection",
	"connection refused",
}

// IsLostServing reports whether err shows a guest that was serving and was
// then lost. Matched by text because the failure crosses the daemon result
// frame as a string with no error identity. Refs: MGIT-99, MGIT-118
func IsLostServing(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	for _, marker := range lostServingMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
