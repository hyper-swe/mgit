// Command npmtree is the GUEST workload for the realistic virtio-fs
// measurement (ADR-010 Gate 2). It replays the file operations an `npm
// install` performs — unpack a dependency tree, then traverse and read it as
// a build or test run would — over the virtio-fs share, using the REAL shape
// of a node_modules tree rather than a uniform synthetic one.
//
// The tree is staged into the guest root by the host, so the guest measures
// the same bytes and the same directory structure a real install produces.
// A true in-guest `npm install` additionally needs node, which is dynamically
// linked against glibc and so needs a guest userspace this image does not
// have — see ADR-010 for that gap. Refs: ADR-010 Gate 2, NFR-17.2
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	// srcTree is the real dependency tree the host staged into the share.
	srcTree = "/tree"
	// dstTree is where the guest unpacks its copy — also on the share, so
	// both the reads and the writes cross virtio-fs.
	dstTree = "/tree-copy"
)

// copyTree replicates src into dst: the file work an unpack performs.
func copyTree(src, dst string) (files int, bytes int64, err error) {
	err = filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil // symlinks in node_modules/.bin are not the measurement
		}
		in, err := os.Open(path) //nolint:gosec // bench-owned tree
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.Create(target) //nolint:gosec // bench-owned tree
		if err != nil {
			return err
		}
		n, err := io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		files++
		bytes += n
		return err
	})
	return files, bytes, err
}

// readTree traverses and reads every file: the work a build or test run does
// over node_modules.
func readTree(root string) (files int, bytes int64, err error) {
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() || !info.Mode().IsRegular() {
			return walkErr
		}
		b, err := os.ReadFile(path) //nolint:gosec // bench-owned tree
		files++
		bytes += int64(len(b))
		return err
	})
	return files, bytes, err
}

func main() {
	if _, err := os.Stat(srcTree); err != nil {
		fmt.Printf("BENCH-ERROR staged tree missing: %v\n", err)
		os.Exit(1)
	}

	start := time.Now()
	wFiles, wBytes, err := copyTree(srcTree, dstTree)
	if err != nil {
		fmt.Printf("BENCH-ERROR unpack: %v\n", err)
		os.Exit(1)
	}
	unpackMS := time.Since(start).Milliseconds()

	start = time.Now()
	rFiles, rBytes, err := readTree(dstTree)
	if err != nil {
		fmt.Printf("BENCH-ERROR traverse: %v\n", err)
		os.Exit(1)
	}
	traverseMS := time.Since(start).Milliseconds()

	fmt.Printf("NPMTREE-RESULT unpack_files=%d unpack_bytes=%d unpack_ms=%d traverse_files=%d traverse_bytes=%d traverse_ms=%d\n",
		wFiles, wBytes, unpackMS, rFiles, rBytes, traverseMS)
	fmt.Println("GUEST: done")
}
