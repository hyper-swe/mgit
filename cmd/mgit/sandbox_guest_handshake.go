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
	return err != nil && (errors.Is(err, model.ErrGuestVersionSkew) ||
		strings.Contains(err.Error(), model.ErrGuestVersionSkew.Error()))
}
