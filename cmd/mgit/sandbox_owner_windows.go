//go:build windows

package main

import "os"

// ownedByUser has no answer on Windows, where mgit v1 has no sandbox and so no
// shared fallback runtime dir; it fails closed. Refs: MGIT-279
func ownedByUser(_ os.FileInfo) bool { return false }
