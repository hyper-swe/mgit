package main

import (
	"errors"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// The daemon's exec result carries text, not Go error identity. Recognize the
// same named refusal after that crossing without treating every handshake
// error as a diagnosed version mismatch.
func isGuestVersionSkew(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, model.ErrGuestVersionSkew) {
		return true
	}
	// Console output is evidence to display, not the daemon's diagnosis.
	// A guest can print the same sentence during an unrelated launch failure.
	cause, _, _ := strings.Cut(err.Error(), "guest console (tail):")
	return strings.Contains(cause, model.ErrGuestVersionSkew.Error())
}
