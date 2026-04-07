package git

import "github.com/go-git/go-git/v5/plumbing"

// hashFromString converts a hex string to a go-git plumbing.Hash.
// Package-private helper used across store implementations.
func hashFromString(s string) plumbing.Hash {
	return plumbing.NewHash(s)
}
