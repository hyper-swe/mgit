//go:build !linux

package guestnet

// NewLinker reports that this platform has no link configurator.
//
// The sandbox guest is always Linux (ADR-005), so this build exists only so
// the package compiles on a developer's host. Returning nil rather than a
// no-op implementation is deliberate: Apply fails closed on a nil Linker, so
// a non-Linux build can never silently "succeed" at configuring a NIC.
// Refs: MGIT-68, ADR-005
func NewLinker() Linker { return nil }
