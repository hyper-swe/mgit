//go:build !unix

package libkrun

// runLifelineHelper has no roles to dispatch on a platform with no sandbox
// backend and no descriptor-passing: TestMain calls it unconditionally, and
// this keeps that one call site free of build tags. Refs: MGIT-103
func runLifelineHelper() (int, bool) { return 0, false }
