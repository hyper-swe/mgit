//go:build !linux

package main

import "errors"

// ensureTmpfs refuses everywhere but the Linux guest: a credential is
// installed only where memory-backed storage can be guaranteed.
func ensureTmpfs(string) error {
	return errors.New("memory-backed storage is only available in the Linux guest")
}
