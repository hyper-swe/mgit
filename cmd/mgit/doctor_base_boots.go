package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/hyper-swe/mgit/internal/doctor"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// resolvePinnedBase resolves this repository's pinned guest base the way the
// daemon's boot does: images.lock's pin, verified against the trust root,
// located through the machine-wide base cache. It returns the host root too,
// for callers that read the lock entry beside it. Refs: MGIT-174, MGIT-230.4
func resolvePinnedBase() (string, images.ResolvedImage, error) {
	none := images.ResolvedImage{}
	hostRoot, err := sandboxHostRoot()
	if err != nil {
		return "", none, fmt.Errorf("no sandbox host root for this repository: %w", err)
	}
	ref, err := images.PinnedRef(hostRoot, defaultGuestBaseName)
	if err != nil {
		return "", none, fmt.Errorf("no guest base registered for this repository: %w", err)
	}
	cache, cacheErr := openBaseCache()
	if cacheErr != nil {
		return "", none, fmt.Errorf("could not open the base cache: %w", cacheErr)
	}
	store, err := images.NewStoreWithBaseCache(hostRoot, func() time.Time { return time.Now().UTC() }, cache)
	if err != nil {
		return "", none, fmt.Errorf("could not open the image store: %w", err)
	}
	resolved, err := store.Resolve(ref)
	if err != nil {
		return "", none, fmt.Errorf("could not resolve the pinned guest base: %w", err)
	}
	return hostRoot, resolved, nil
}

// inspectBaseShape reads the registered base's shape for base/boots: a
// directory, or a kernel plus a rootfs image. The root is stat'ed, not
// inferred from the registering command, so a base registered any other way
// reads the same. Refs: MGIT-230.4
func inspectBaseShape() (doctor.BaseShape, error) {
	_, resolved, err := resolvePinnedBase()
	if err != nil {
		return doctor.BaseShape{}, err
	}
	info, err := os.Stat(resolved.RootfsPath)
	if err != nil {
		return doctor.BaseShape{}, fmt.Errorf("the guest base's root: %w", err)
	}
	return doctor.BaseShape{
		Name:       defaultGuestBaseName,
		KernelPath: resolved.KernelPath,
		RootfsPath: resolved.RootfsPath,
		RootIsDir:  info.IsDir(),
	}, nil
}

// memoVMM asks the daemon once per doctor run: daemon/vmm and base/boots
// put the same question to the same binary, and the probe starts a process.
// Refs: MGIT-230.4
func memoVMM(probe func(context.Context) (doctor.DaemonVMM, error)) func(context.Context) (doctor.DaemonVMM, error) {
	var once sync.Once
	var v doctor.DaemonVMM
	var err error
	return func(ctx context.Context) (doctor.DaemonVMM, error) {
		once.Do(func() { v, err = probe(ctx) })
		return v, err
	}
}
