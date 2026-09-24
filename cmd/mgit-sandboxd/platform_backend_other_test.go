//go:build !linux && !darwin

package main

// wantLinkedVMM is what --vmm must report on a platform with no sandbox
// backend (Windows and the rest run core mgit without containment), stated
// here from the build tags above rather than read from the code under test.
// Refs: MGIT-229, ADR-006
const wantLinkedVMM = "none"
