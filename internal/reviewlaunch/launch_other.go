//go:build !linux

package reviewlaunch

import (
	"io"
)

// Config describes one review launch.
type Config struct {
	Entrypoint string
	SHA256     string
	Argv       []string
	Dir        string
	DropTo     *Identity
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

// Identity is the unprivileged user and group the entrypoint runs as.
type Identity struct{ UID, GID int }

// MaybeStage reports false: there is no launcher stage off Linux.
func MaybeStage() bool { return false }

// Run refuses: the review launcher is a Linux guest component.
func Run(Config) (int, error) {
	return 0, refuse(CodeProfileUnavailable, "the review launcher runs only in a Linux guest")
}
