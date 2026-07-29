package images

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A libkrun guest base is a DIRECTORY, not a rootfs file: libkrunfw supplies
// the kernel and the guest root is shared over virtio-fs. Pinning it needs a
// digest over the whole tree, so that "the base cannot be swapped silently
// under a task" (MGIT-61.15) holds for trees exactly as it already does for
// files. Refs: FR-17.17, FR-17.29, MGIT-61.15, ADR-002

// writeTree materializes a tree from a path->content map, creating parents.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTreeDigest_IsStableAndContentAddressed(t *testing.T) {
	files := map[string]string{
		"sbin/mgit-guest": "init",
		"bin/mgit":        "cli",
		"etc/group":       "root:x:0:",
	}
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, files)
	writeTree(t, b, files)

	da, err := TreeDigest(a)
	if err != nil {
		t.Fatalf("TreeDigest: %v", err)
	}
	db, err := TreeDigest(b)
	if err != nil {
		t.Fatalf("TreeDigest: %v", err)
	}

	if !strings.HasPrefix(da, "sha256:") {
		t.Errorf("digest %q is not in the sha256:<hex> form the lock uses", da)
	}
	// Two trees with identical content must pin identically, or a rebuild of
	// the same base would look like a substitution.
	if da != db {
		t.Errorf("identical trees hashed differently:\n  %s\n  %s", da, db)
	}
	// And it must be STABLE across calls (no map-iteration nondeterminism).
	for range 5 {
		again, err := TreeDigest(a)
		if err != nil {
			t.Fatal(err)
		}
		if again != da {
			t.Fatalf("digest is not deterministic: %s then %s", da, again)
		}
	}
}

func TestTreeDigest_DetectsEveryKindOfSubstitution(t *testing.T) {
	base := map[string]string{"sbin/mgit-guest": "init", "bin/mgit": "cli"}

	tests := []struct {
		name   string
		mutate func(t *testing.T, root string)
	}{
		{
			// SAME LENGTH on purpose: a digest that only covered names and
			// sizes would pass this, and swapping a binary for one of equal
			// size is exactly the substitution a pin must catch.
			name: "changed_content_same_length",
			mutate: func(t *testing.T, root string) {
				writeTree(t, root, map[string]string{"bin/mgit": "EVL"}) // "cli" is also 3 bytes
			},
		},
		{
			// The attack this exists to stop: an extra binary appearing in a
			// base that a task already pinned.
			name: "added_file",
			mutate: func(t *testing.T, root string) {
				writeTree(t, root, map[string]string{"bin/backdoor": "evil"})
			},
		},
		{
			name: "removed_file",
			mutate: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "bin", "mgit")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Same bytes, different path: content-only hashing would miss it.
			name: "renamed_file",
			mutate: func(t *testing.T, root string) {
				if err := os.Rename(filepath.Join(root, "bin", "mgit"),
					filepath.Join(root, "bin", "mgit2")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// A base whose supervisor becomes executable-by-nobody, or a data
			// file that becomes executable, is a different base.
			name: "changed_exec_bit",
			mutate: func(t *testing.T, root string) {
				//nolint:gosec // G302: flipping the exec bit IS the mutation under test
				if err := os.Chmod(filepath.Join(root, "bin", "mgit"), 0o750); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, base)
			before, err := TreeDigest(root)
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(t, root)
			after, err := TreeDigest(root)
			if err != nil {
				t.Fatal(err)
			}
			if before == after {
				t.Errorf("%s did not change the digest; a swapped base would go unnoticed", tt.name)
			}
		})
	}
}

func TestTreeDigest_IgnoresIrrelevantMetadata(t *testing.T) {
	// mtimes differ between a fresh extraction and a copy of the same base.
	// Pinning them would make every re-materialization look like tampering,
	// which trains people to ignore the check.
	root := t.TempDir()
	writeTree(t, root, map[string]string{"bin/mgit": "cli"})
	before, err := TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "bin", "mgit")
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, past, past); err != nil {
		t.Fatal(err)
	}
	after, err := TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Error("mtime changed the digest; re-materializing a base would look like a substitution")
	}
}

func TestTreeDigest_RefusesWhatItCannotPin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	tests := []struct {
		name    string
		build   func(t *testing.T, root string)
		wantErr string
	}{
		{
			name:    "not_a_directory",
			build:   func(t *testing.T, root string) {},
			wantErr: "not a directory",
		},
		{
			// A symlink out of the tree makes the digest a claim about files
			// the base does not contain — it would pin a moving target.
			name: "symlink_escaping_the_tree",
			build: func(t *testing.T, root string) {
				outside := filepath.Join(t.TempDir(), "secret")
				if err := os.WriteFile(outside, []byte("host"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "escapes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "not_a_directory" {
				f := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := TreeDigest(f); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("TreeDigest(file) = %v, want error mentioning %q", err, tt.wantErr)
				}
				return
			}
			root := t.TempDir()
			writeTree(t, root, map[string]string{"bin/mgit": "cli"})
			tt.build(t, root)
			if _, err := TreeDigest(root); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("TreeDigest = %v, want error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestTreeDigest_InternalSymlinkIsPinned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	// Symlinks WITHIN the tree are normal in a userspace base (bin/sh ->
	// busybox), so they must be pinned rather than refused — and retargeting
	// one must change the digest.
	root := t.TempDir()
	writeTree(t, root, map[string]string{"bin/busybox": "bb", "bin/other": "oo"})
	link := filepath.Join(root, "bin", "sh")
	if err := os.Symlink("busybox", link); err != nil {
		t.Fatal(err)
	}
	before, err := TreeDigest(root)
	if err != nil {
		t.Fatalf("an internal symlink must be pinnable, not refused: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", link); err != nil {
		t.Fatal(err)
	}
	after, err := TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("retargeting an internal symlink did not change the digest")
	}
}

// TestVerifyContentDigest_HandlesBothFilesAndTrees pins the dispatch. A base
// is only pinned if verification runs on EVERY resolve — a digest recorded
// once and never re-checked protects nothing. Refs: FR-17.17, FR-17.29
func TestVerifyContentDigest_HandlesBothFilesAndTrees(t *testing.T) {
	t.Run("file_unchanged_behavior", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "rootfs.img")
		if err := os.WriteFile(f, []byte("rootfs bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		digest, err := ComputeDigest(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyContentDigest(f, digest); err != nil {
			t.Errorf("a matching file digest must verify: %v", err)
		}
		if err := verifyContentDigest(f, "sha256:"+strings.Repeat("0", 64)); err == nil {
			t.Error("a mismatched file digest must fail")
		}
	})

	t.Run("directory_base", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"sbin/mgit-guest": "init"})
		digest, err := TreeDigest(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyContentDigest(root, digest); err != nil {
			t.Errorf("a matching tree digest must verify: %v", err)
		}
		// The substitution this exists to catch, checked through the same
		// entry point a launch uses.
		writeTree(t, root, map[string]string{"bin/backdoor": "evil"})
		err = verifyContentDigest(root, digest)
		if err == nil {
			t.Fatal("a modified base verified against its old digest")
		}
		if !strings.Contains(err.Error(), "hashes to") {
			t.Errorf("error %q does not report the mismatch", err)
		}
	})
}

// TestBuildBaseEntry_PinsTheTreeAndCarriesNoKernel covers the registration
// shape for a libkrun base. Refs: MGIT-61.15, ADR-010
func TestBuildBaseEntry_PinsTheTreeAndCarriesNoKernel(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sbin/mgit-guest": "init", "bin/mgit": "cli"})

	entry, err := BuildBaseEntry(root)
	if err != nil {
		t.Fatalf("BuildBaseEntry: %v", err)
	}
	want, err := TreeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Digest != want {
		t.Errorf("entry digest = %q, want the tree digest %q", entry.Digest, want)
	}
	if entry.RootfsPath != root {
		t.Errorf("rootfs path = %q, want the base dir %q", entry.RootfsPath, root)
	}
	// libkrunfw supplies the kernel, so claiming one would be a lie the
	// verifier would then try to check.
	if entry.KernelPath != "" || entry.KernelDigest != "" {
		t.Errorf("a libkrun base must carry no kernel, got path=%q digest=%q",
			entry.KernelPath, entry.KernelDigest)
	}

	t.Run("signs_and_resolves_through_the_normal_path", func(t *testing.T) {
		hostRoot := t.TempDir()
		priv, err := GenerateTrustRoot(context.Background(), hostRoot, noopAuditor{})
		if err != nil {
			t.Fatalf("GenerateTrustRoot: %v", err)
		}
		ref, err := Register(hostRoot, "base", Sign("base", entry, priv), priv)
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		store, err := NewStore(hostRoot, func() time.Time { return time.Unix(0, 0).UTC() })
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		resolved, err := store.Resolve(ref)
		if err != nil {
			t.Fatalf("a registered base must resolve: %v", err)
		}
		if resolved.RootfsPath != root {
			t.Errorf("resolved rootfs = %q, want %q", resolved.RootfsPath, root)
		}

		// And the pin must BITE: mutate the tree, re-resolve, expect refusal.
		writeTree(t, root, map[string]string{"bin/backdoor": "evil"})
		if _, err := store.Resolve(ref); err == nil {
			t.Fatal("a mutated base still resolved; the tree pin does not protect a launch")
		}
	})
}

// noopAuditor satisfies TrustRootAuditor for tests that only need a key.
type noopAuditor struct{}

func (noopAuditor) RecordTrustRootChange(context.Context, string) error { return nil }

// TestResolve_HalfSpecifiedKernel_IsRefused pins the middle branch: a base
// legitimately has no kernel, but an entry claiming a path with no digest (or
// the reverse) is malformed and must not resolve. Refs: FR-17.17
func TestResolve_HalfSpecifiedKernel_IsRefused(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sbin/mgit-guest": "init"})
	entry, err := BuildBaseEntry(root)
	if err != nil {
		t.Fatal(err)
	}
	entry.KernelPath = "/no/such/kernel" // digest still empty

	hostRoot := t.TempDir()
	priv, err := GenerateTrustRoot(context.Background(), hostRoot, noopAuditor{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := Register(hostRoot, "half", Sign("half", entry, priv), priv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(hostRoot, func() time.Time { return time.Unix(0, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ref); err == nil {
		t.Fatal("a half-specified kernel must be refused, not guessed at")
	}
}

// TestResolve_TamperedSource_IsRefused pins provenance to the signature.
//
// A Source that is recorded but NOT signed is worse than no Source at all: an
// entry could be edited to claim a base came from debian:12 when it came from
// somewhere else entirely, and every verification would still pass. An audit
// record that can be rewritten without detection is decoration.
// Refs: MGIT-61.15, FR-17.17
func TestResolve_TamperedSource_IsRefused(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sbin/mgit-guest": "init"})
	entry, err := BuildBaseEntry(root)
	if err != nil {
		t.Fatal(err)
	}
	entry.Source = "registry-1.docker.io/library/debian:12"

	hostRoot := t.TempDir()
	priv, err := GenerateTrustRoot(context.Background(), hostRoot, noopAuditor{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := Register(hostRoot, "base", Sign("base", entry, priv), priv)
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite only the provenance, leaving digest and signature untouched.
	lockPath := filepath.Join(hostRoot, "images.lock")
	raw, err := os.ReadFile(lockPath) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw),
		"library/debian:12", "library/totally-fine:12", 1)
	if tampered == string(raw) {
		t.Fatal("the source was never written to images.lock, so nothing was tested")
	}
	if err := os.WriteFile(lockPath, []byte(tampered), 0o600); err != nil { //nolint:gosec // lockPath is this test's own t.TempDir
		t.Fatal(err)
	}

	store, err := NewStore(hostRoot, func() time.Time { return time.Unix(0, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ref); err == nil {
		t.Fatal("provenance was edited without breaking verification; Source is not signed")
	}
}

// TestPinnedRef_ReturnsWhatLaunchShouldBoot covers launch-time base selection:
// once a base is registered, booting it must not require the user to retype a
// digest they never chose. Refs: MGIT-61.15
func TestPinnedRef_ReturnsWhatLaunchShouldBoot(t *testing.T) {
	hostRoot := t.TempDir()
	priv, err := GenerateTrustRoot(context.Background(), hostRoot, noopAuditor{})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing registered yet: the caller must be able to tell "no base" from
	// "broken lock", because only the first has a one-command remedy.
	if _, err := PinnedRef(hostRoot, "base"); !errors.Is(err, ErrNoSuchImage) {
		t.Fatalf("PinnedRef with no lock = %v, want ErrNoSuchImage", err)
	}

	root := t.TempDir()
	writeTree(t, root, map[string]string{"sbin/mgit-guest": "init"})
	entry, err := BuildBaseEntry(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Register(hostRoot, "base", Sign("base", entry, priv), priv)
	if err != nil {
		t.Fatal(err)
	}

	got, err := PinnedRef(hostRoot, "base")
	if err != nil {
		t.Fatalf("PinnedRef after registering: %v", err)
	}
	if got != want {
		t.Errorf("PinnedRef = %q, want the reference Register handed back, %q", got, want)
	}
	if _, err := PinnedRef(hostRoot, "other"); !errors.Is(err, ErrNoSuchImage) {
		t.Errorf("PinnedRef for an unregistered name = %v, want ErrNoSuchImage", err)
	}
}
