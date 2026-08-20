//go:build !(darwin && cgo)

package vzf

// refuseUnenforceableNetwork is the non-darwin twin of the darwin
// implementation. On a build where vzf cannot run at all, no mode is
// enforceable by it — but the manager is still constructed by tests and by
// SelectBackend's refusal path, and answering "everything is fine" from a
// backend that cannot start is worse than answering nothing. The launch itself
// fails with ErrSandboxBackendUnavailable, which is the accurate error, so this
// declines to add a second, less accurate one. Refs: MGIT-111
func refuseUnenforceableNetwork(string) error { return nil }
