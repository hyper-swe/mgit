package main

import (
	"errors"
	"log/slog"
	"os"

	"github.com/hyper-swe/mgit/internal/reviewbind"
	"github.com/hyper-swe/mgit/internal/sandboxd/reviewsvc"
)

// reviewBindings builds the review service's bindings: the operator's map
// store, the egress policy and the credential slot. credFD is an inherited
// descriptor holding one credential (or -1 for none) and credRef names it; the
// bytes are read once here, never put in an argument or the environment. With
// no credential or egress enforcer the binding refuses any map that asks for
// one, so the daemon runs no review without what the map named.
func reviewBindings(credFD int, credRef string, logger *slog.Logger) (reviewsvc.Bindings, error) {
	cfg := reviewbind.Config{}
	if credFD >= 0 {
		if credRef == "" {
			return nil, errors.New("review credential: an fd needs a ref")
		}
		f := os.NewFile(uintptr(credFD), "review-credential")
		if f == nil {
			return nil, errors.New("review credential: the descriptor is not usable")
		}
		defer f.Close()
		src, err := reviewbind.StaticSource(credRef, f)
		if err != nil {
			return nil, errors.New("review credential: the descriptor could not be read")
		}
		cfg.Source = src
		if logger != nil {
			logger.Info("review credential loaded", "ref", credRef)
		}
	}
	return reviewbind.New(cfg), nil
}
