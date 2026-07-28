// Command fsbench is the GUEST workload for the virtiofs performance gate
// (ADR-010 Gate 2). It runs inside a real libkrun microVM whose root is a
// host directory shared over virtiofs, and performs an npm-install-class
// workload — many thousands of small files — reporting timings the host
// asserts on. Refs: MGIT-61.6, ADR-010 Gate 2
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// workDir is on the VIRTIOFS share (the guest root IS the shared host
	// directory), so every operation below crosses the virtio-fs boundary —
	// which is the thing being measured.
	workDir = "/bench"
	// fileCount is npm-install-class in shape if not in scale: node_modules
	// runs to tens of thousands of small files. 4000 keeps one boot short
	// while still amortising per-file cost over enough samples to be
	// meaningful, and the per-file figure is what extrapolates.
	fileCount = 4000
	// fileSize is a small-file payload, the regime virtio-fs is worst at
	// (per-file syscall overhead dominates, not bandwidth).
	fileSize = 512
)

func main() {
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}

	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fmt.Printf("BENCH-ERROR mkdir: %v\n", err)
		os.Exit(1)
	}

	// CREATE: the dominant cost of an install (unpack).
	start := time.Now()
	for i := 0; i < fileCount; i++ {
		dir := filepath.Join(workDir, fmt.Sprintf("pkg%03d", i%64))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Printf("BENCH-ERROR mkdir %s: %v\n", dir, err)
			os.Exit(1)
		}
		name := filepath.Join(dir, fmt.Sprintf("f%05d.js", i))
		if err := os.WriteFile(name, payload, 0o644); err != nil {
			fmt.Printf("BENCH-ERROR write %s: %v\n", name, err)
			os.Exit(1)
		}
	}
	writeMS := time.Since(start).Milliseconds()

	// STAT+READ: the dominant cost of a build/test run over node_modules.
	start = time.Now()
	var bytesRead int
	err := filepath.Walk(workDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // bench-owned path
		bytesRead += len(b)
		return err
	})
	if err != nil {
		fmt.Printf("BENCH-ERROR walk: %v\n", err)
		os.Exit(1)
	}
	readMS := time.Since(start).Milliseconds()

	fmt.Printf("BENCH-RESULT files=%d write_ms=%d read_ms=%d bytes_read=%d\n",
		fileCount, writeMS, readMS, bytesRead)
	fmt.Println("GUEST: done")
}
