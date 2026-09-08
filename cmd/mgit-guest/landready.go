package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// landReadyGate decides whether an exec left the guest's private store with
// something new to land. It fingerprints the store's HEAD and refs — the
// files a commit moves — and reports a change only when the fingerprint
// differs from the last one it saw.
//
// WHY. The land-ready notify fired after EVERY exec the guest served, by
// design: a redundant trigger is a host no-op land. But a launch runs its
// own setup execs before the agent's first command, so every boot cost the
// host three full land passes (pull + verify) with nothing to land, and every
// later command one more (MGIT-199). Landing is for what the guest
// committed; the store's refs are exactly that record. The baseline is the
// store as delivered: what the guest booted with, the host already knows.
// Refs: MGIT-199, MGIT-11.10.11
type landReadyGate struct {
	mu       sync.Mutex
	storeDir string
	last     string
}

// newLandReadyGate baselines the gate on the store as it is now.
func newLandReadyGate(storeDir string) *landReadyGate {
	g := &landReadyGate{storeDir: storeDir}
	g.last = storeFingerprint(storeDir)
	return g
}

// changed reports whether the store's HEAD or refs differ from the last
// look, and remembers the new state. An absent store never changes.
func (g *landReadyGate) changed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := storeFingerprint(g.storeDir)
	if now == g.last {
		return false
	}
	g.last = now
	return true
}

// storeFingerprint hashes HEAD, packed-refs and every file under refs/ by
// path and content. It reads a handful of small files; a missing store
// hashes to the empty string.
func storeFingerprint(storeDir string) string {
	var paths []string
	for _, p := range []string{"HEAD", "packed-refs"} {
		paths = append(paths, filepath.Join(storeDir, p))
	}
	_ = filepath.WalkDir(filepath.Join(storeDir, "refs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths)
	h := sha256.New()
	seen := false
	for _, p := range paths {
		data, err := os.ReadFile(p) //nolint:gosec // fixed names under the guest's own private store
		if err != nil {
			continue
		}
		seen = true
		rel := strings.TrimPrefix(p, storeDir)
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	if !seen {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
