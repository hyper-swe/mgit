package main

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// guestBaseMountDirs are the mount points the in-guest supervisor needs at
// boot: it mounts /proc and /dev, a tmpfs on /tmp, and overlays a writable
// root using /mnt as the scratch for the upper/work/newroot layers.
//
// A tree missing any of them boots and then dies with a bare "no such file or
// directory" naming the mount, which says nothing about the tree being
// incomplete. The old ext4 image builder created them, so nothing had to
// check; a bring-your-own directory has no such guarantee.
// Refs: FR-17.3, FR-17.17, MGIT-61.15
var guestBaseMountDirs = []string{"proc", "dev", "tmp", "mnt"}

// sandboxBaseCmd manages the per-repo guest base: the Linux userspace tree
// every sandbox for this repository boots from, read-only, with each VM
// getting its own writable overlay.
//
// Under libkrun a base is just a DIRECTORY — libkrunfw supplies the kernel
// and the guest root is shared over virtio-fs — so there is no image to
// build, no kernel to pin, and composing one is a filesystem operation.
// Refs: MGIT-61.15, ADR-010
func sandboxBaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "base",
		Short: "Manage the per-repo guest base the sandbox boots from",
		Long: "The guest base is a Linux userspace tree shared read-only into every\n" +
			"sandbox for this repository. Point mgit at a tree you built or extracted,\n" +
			"and it is pinned by content digest and signed into images.lock so it\n" +
			"cannot change under a running task without being noticed.\n\n" +
			"A composed base lives in a machine-wide, content-addressed cache — under\n" +
			"XDG_CACHE_HOME on Linux, ~/Library/Caches on macOS, or MGIT_BASE_CACHE —\n" +
			"never inside your repository. Your repo keeps the pinned digest and no\n" +
			"bytes, every repo on the machine shares one copy of identical bytes, and\n" +
			"recomposing publishes a NEW entry rather than rewriting the one somebody\n" +
			"else pinned.",
	}
	cmd.AddCommand(sandboxBaseSetCmd(), sandboxBaseFromCmd())
	return cmd
}

// sandboxBaseSetCmd validates a userspace tree, injects mgit + mgit-guest
// into it, and registers it as this repo's pinned, signed guest base.
func sandboxBaseSetCmd() *cobra.Command {
	var opts composeOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "set <dir>",
		Short: "Use a directory as this repo's guest base (validates, injects mgit, pins and signs it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, baseDir, err := setRepoGuestBase(cmd, args[0], opts)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"image_ref": ref})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered guest base %s\n  from %s\n", ref, baseDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.name, "name", "base", "name to register the base under in images.lock")
	cmd.Flags().StringVar(&opts.guestBinDir, "guest-bin-dir", "",
		"directory holding linux builds of mgit and mgit-guest to inject; "+
			"defaults to the ones shipped with this install")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the digest-pinned reference as JSON")
	return cmd
}

// setRepoGuestBase pins a directory the USER owns as this repo's guest base.
//
// Unlike `base from`, nothing is copied: the tree stays where the operator put
// it and images.lock records its path alongside the digest. That is the
// bring-your-own contract — which is also why a tree inside the repository is
// refused rather than quietly relocated. Refs: MGIT-61.15, MGIT-147
func setRepoGuestBase(cmd *cobra.Command, dir string, opts composeOptions) (ref, baseDir string, err error) {
	baseDir, err = filepath.Abs(dir)
	if err != nil {
		return "", "", fmt.Errorf("base set: %w", err)
	}
	hostRoot, err := sandboxHostRoot()
	if err != nil {
		return "", "", err
	}
	if err := refuseInRepoBaseTree(baseDir, hostRoot); err != nil {
		return "", "", err
	}
	// The signing key stays host-side and never enters a guest (SEC-01). First
	// run has no trust root, and telling a user to go and make one — after mgit
	// told them to run THIS command — is guidance that leads into a second
	// wall. An existing key is reused, never rotated. Refs: MGIT-65, FR-17.38
	priv, err := images.EnsureSigningKey(cmd.Context(), hostRoot,
		printTrustRootAuditor{w: cmd.OutOrStdout()})
	if err != nil {
		return "", "", fmt.Errorf("base set: %w", err)
	}
	if err := validateBaseTree(baseDir); err != nil {
		return "", "", err
	}
	// Inject BEFORE pinning: the digest must cover the binaries we added, or
	// the pin would describe a tree that never boots.
	if err := injectGuestBinaries(baseDir, opts.guestBinDir, hostExecutablePath()); err != nil {
		return "", "", err
	}
	entry, err := images.BuildBaseEntry(baseDir)
	if err != nil {
		return "", "", fmt.Errorf("base set: %w", err)
	}
	ref, err = images.Register(hostRoot, opts.name, entry, priv)
	if err != nil {
		return "", "", fmt.Errorf("base set: %w", err)
	}
	return ref, baseDir, nil
}

// validateBaseTree rejects a tree that could not boot, naming what is missing
// and how to fix it. This runs before anything is injected or pinned, so a
// malformed base is a clear error rather than a guest that dies mid-boot.
// Refs: FR-17.3, MGIT-61.15
func validateBaseTree(baseDir string) error {
	info, err := os.Stat(baseDir)
	if err != nil {
		return fmt.Errorf("guest base %s: %w", baseDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf(
			"guest base %s is not a directory — libkrun shares the guest "+
				"root over virtio-fs, so a base is a directory tree, not an image", baseDir)
	}
	var missing []string
	for _, d := range guestBaseMountDirs {
		if fi, err := os.Stat(filepath.Join(baseDir, d)); err != nil || !fi.IsDir() {
			missing = append(missing, "/"+d)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"guest base %s is missing the mount points the guest supervisor "+
				"needs at boot: %v. Create them first:\n  mkdir -p %s%v",
			baseDir, missing, baseDir, missing)
	}
	return nil
}

// sandboxBaseFromCmd composes this repo's guest base from an OCI image.
//
// The user pulls the image, so mgit redistributes nothing — no kernel, no
// busybox, no GPL corresponding-source obligation. What we contribute is our
// own Apache-2.0 binaries, injected on top. Refs: MGIT-61.15, ADR-010
func sandboxBaseFromCmd() *cobra.Command {
	var name, guestBinDir string
	var plainHTTP, asJSON bool
	cmd := &cobra.Command{
		Use:   "from <oci-ref>",
		Short: "Compose this repo's guest base from an OCI image (e.g. debian:12, node:22-slim)",
		Long: "Pulls a public OCI image, composes its layers into a guest base in this\n" +
			"machine's base cache, injects mgit and mgit-guest, then pins the composed\n" +
			"tree by content digest and signs that digest into this repo's images.lock.\n" +
			"No base bytes are written inside the repository.\n\n" +
			"The tag is resolved to a digest ONCE and recorded as provenance: a tag can\n" +
			"point twice, a digest cannot. Re-composing the same tag onto different\n" +
			"bytes produces a new cache entry and says what changed; it never replaces\n" +
			"what you had pinned.\n\n" +
			"The image supplies the Linux userspace your agent's toolchain needs; mgit\n" +
			"supplies the supervisor and the CLI. Because YOU pull the image, mgit\n" +
			"redistributes nothing.\n\n" +
			"Loading arbitrary tooling into the guest is safe because the guest is the\n" +
			"UNTRUSTED side — that is what the VM boundary is for, and a poisoned base\n" +
			"burns a throwaway microVM. The host store, egress policy, land airlock and\n" +
			"attestation signing are all enforced host-side and are unaffected by what\n" +
			"the base contains.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := composeOptions{name: name, guestBinDir: guestBinDir, plainHTTP: plainHTTP}
			res, err := composeBaseFromImage(cmd, args[0], opts)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(composeJSON(res))
			}
			reportComposition(cmd.OutOrStdout(), res)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "base", "name to register the base under in images.lock")
	cmd.Flags().StringVar(&guestBinDir, "guest-bin-dir", "",
		"directory holding linux builds of mgit and mgit-guest to inject; "+
			"defaults to the ones shipped with this install")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false,
		"talk to the registry over http (local mirrors and tests only)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output the digest-pinned reference as JSON")
	return cmd
}

// composeBaseFromImage pulls an OCI image into a private staging tree in the
// machine-wide cache, finishes it into a bootable guest base, and publishes it
// under its content digest.
//
// NOTHING IS COMPOSED INSIDE THE REPOSITORY. The staging tree lives in the
// cache so publishing is a rename, and the repository ends up holding a
// digest and no bytes. Refs: MGIT-147, MGIT-61.15
func composeBaseFromImage(cmd *cobra.Command, refArg string, opts composeOptions) (composeResult, error) {
	ref, err := guestbase.ParseRef(refArg)
	if err != nil {
		return composeResult{}, err
	}
	env, err := openComposeEnv(cmd)
	if err != nil {
		return composeResult{}, err
	}

	staging, err := env.cache.Stage()
	if err != nil {
		return composeResult{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = env.cache.Discard(staging)
		}
	}()

	resolved, err := guestbase.Pull(cmd.Context(), ref, staging, guestbase.PullOptions{
		PlainHTTP: opts.plainHTTP,
		Progress:  func(msg string) { _, _ = fmt.Fprintf(env.out, "  %s\n", msg) },
	})
	if err != nil {
		return composeResult{}, err
	}
	if err := finishComposedTree(staging, resolved, opts, env.out); err != nil {
		return composeResult{}, err
	}

	cached, err := env.cache.Commit(staging, images.TreeDigest)
	if err != nil {
		return composeResult{}, err
	}
	published = true
	return registerComposedBase(env.hostRoot, cached, resolved.String(), opts,
		signWith(env.priv), func() time.Time { return time.Now().UTC() })
}

// composeEnv is everything a composition needs before it touches a registry:
// where to register, where to stage, what to sign with, and where to report.
type composeEnv struct {
	hostRoot string
	cache    *basecache.Cache
	priv     ed25519.PrivateKey
	out      io.Writer
}

// openComposeEnv resolves that environment, migrating an older mgit's in-tree
// base out of the repository on the way. Refs: MGIT-147, MGIT-65, FR-17.38
func openComposeEnv(cmd *cobra.Command) (composeEnv, error) {
	hostRoot, err := sandboxHostRoot()
	if err != nil {
		return composeEnv{}, err
	}
	out := cmd.OutOrStdout()
	cache, err := openBaseCache()
	if err != nil {
		return composeEnv{}, err
	}
	// An older mgit left its base inside the repo; take it out before adding
	// another.
	if err := migrateInTreeBase(hostRoot, cache, out); err != nil {
		return composeEnv{}, err
	}
	// First run has no trust root, and telling a user to go and make one —
	// after mgit told them to run THIS command — is guidance that leads into a
	// second wall. An existing key is reused, never rotated.
	priv, err := images.EnsureSigningKey(cmd.Context(), hostRoot, printTrustRootAuditor{w: out})
	if err != nil {
		return composeEnv{}, fmt.Errorf("base from: %w", err)
	}
	return composeEnv{hostRoot: hostRoot, cache: cache, priv: priv, out: out}, nil
}

// finishComposedTree turns freshly extracted image layers into a bootable
// guest base: mount points, sanity checks, and our own binaries on top.
func finishComposedTree(staging string, source fmt.Stringer, opts composeOptions, out io.Writer) error {
	// Images vary in which empty mount points they ship, and this directory is
	// one WE composed — so creating them is part of composing, not a reason to
	// reject an otherwise good image. `base set` refuses instead, because
	// there the tree is the user's and mgit has no business writing into it.
	if err := ensureGuestMountDirs(staging); err != nil {
		return err
	}
	if err := assertLinuxUserspace(staging, source); err != nil {
		return err
	}
	if err := validateBaseTree(staging); err != nil {
		return err
	}
	if warning := checkLibcCoherence(staging); warning != "" {
		_, _ = fmt.Fprintf(out, "  warning: %s\n", warning)
	}
	// Inject BEFORE the digest is taken: the pin must cover the binaries we
	// added, or it would describe a tree that never boots.
	return injectGuestBinaries(staging, opts.guestBinDir, hostExecutablePath())
}

// signWith adapts a signing key to the signFunc the compose flow takes.
func signWith(priv ed25519.PrivateKey) signFunc {
	return func(hostRoot, name string, entry images.Entry) (string, error) {
		ref, err := images.Register(hostRoot, name, entry, priv)
		if err != nil {
			return "", fmt.Errorf("base %s: %w", name, err)
		}
		return ref, nil
	}
}

// refuseInRepoBaseTree refuses to pin a base that lives inside the repository.
//
// A pinned in-repo tree is the defect MGIT-147 removes, wearing a different
// hat: hundreds of megabytes that every host test command walking the repo
// trips over, and bytes that any commit or clean could move under a pin. It
// is refused rather than copied because `base set` means "use THIS tree", and
// quietly using a copy somewhere else would make the command a liar.
// Refs: MGIT-147
func refuseInRepoBaseTree(baseDir, hostRoot string) error {
	repoRoot := filepath.Dir(filepath.Dir(hostRoot))
	// A path Rel cannot express (a different volume) is outside by definition,
	// so it is folded into the same answer rather than raised.
	rel, relErr := filepath.Rel(repoRoot, baseDir)
	outside := relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	if outside {
		return nil
	}
	return fmt.Errorf(
		"guest base %s is inside the repository %s.\n\n"+
			"A base is hundreds of megabytes and thousands of files; inside the repo it is "+
			"walked by every test command that walks your tree (`gofmt -l .`, `go vet ./...`, "+
			"a linter), and mgit would be breaking your repo's own checks.\n\n"+
			"Either move the tree outside the repository and re-run this, or let mgit compose "+
			"and cache one for you:\n  mgit sandbox base from debian:12", baseDir, repoRoot)
}

// ensureGuestMountDirs creates the empty mount points the in-guest supervisor
// needs. Refs: FR-17.3, MGIT-61.15
func ensureGuestMountDirs(baseDir string) error {
	for _, d := range guestBaseMountDirs {
		if err := os.MkdirAll(filepath.Join(baseDir, d), 0o755); err != nil { //nolint:gosec // guest mount points must be world-traversable, as in any rootfs
			return fmt.Errorf("guest base: create mount point /%s: %w", d, err)
		}
	}
	return nil
}

// guestShells are the places a usable base keeps its shell. Any one of them is
// enough; which one it is does not matter to us.
var guestShells = []string{"bin/sh", "usr/bin/sh", "bin/bash", "bin/busybox"}

// assertLinuxUserspace refuses a scratch or distroless image.
//
// Such an image is a single binary, not a userspace: mgit-guest would boot in
// it, and then every agent command would fail because there is no shell to run
// it with. Catching that here — while the user is still looking at the pull
// they just typed — is far kinder than an exec failure inside a task hours
// later. Refs: MGIT-61.15, FR-17.11
func assertLinuxUserspace(baseDir string, source fmt.Stringer) error {
	for _, sh := range guestShells {
		// Lstat, not Stat: bin/sh is usually a symlink, and on a tree we have
		// not chrooted into, its target does not resolve from here.
		if _, err := os.Lstat(filepath.Join(baseDir, sh)); err == nil {
			return nil
		}
	}
	return fmt.Errorf(
		"the image %s has no shell (looked for %s), so it is not a Linux userspace "+
			"a sandbox can run commands in.\n\n"+
			"Scratch and distroless images cannot be guest bases; distro images "+
			"(debian:12, ubuntu:24.04, alpine:3.20) and language images "+
			"(node:22, python:3.12, golang:1.23) all work",
		source, strings.Join(guestShells, ", "))
}

// globAny reports whether name matches anything in the tree's library
// directories, at either the top level or under a multiarch tuple.
func globAny(baseDir, name string) bool {
	for _, pattern := range []string{
		filepath.Join(baseDir, "lib*", name),
		filepath.Join(baseDir, "lib*", "*", name),
		filepath.Join(baseDir, "usr", "lib*", name),
		filepath.Join(baseDir, "usr", "lib*", "*", name),
	} {
		if hits, _ := filepath.Glob(pattern); len(hits) > 0 {
			return true
		}
	}
	return false
}

// checkLibcCoherence warns when a base's C library will not match the
// toolchains a user is likely to add to it.
//
// It WARNS rather than refuses: a musl base is perfectly valid, and plenty of
// workloads are static or musl-native. What is not valid is silence — an
// alpine base with a glibc-linked toolchain fails at runtime with "no such
// file or directory" naming the INTERPRETER, which is one of the most
// confusing errors in Linux. Refs: MGIT-61.15 req 6
func checkLibcCoherence(baseDir string) string {
	// Both libraries live one or two levels under a lib directory, the second
	// level being the multiarch tuple (lib/x86_64-linux-gnu/libc.so.6).
	musl := globAny(baseDir, "ld-musl-*")
	glibc := globAny(baseDir, "libc.so.6")
	switch {
	case musl && !glibc:
		return "this base uses musl (alpine-style). Toolchains built against " +
			"glibc will fail inside it with a confusing \"no such file or directory\" " +
			"naming the dynamic loader. Prefer musl-native or statically linked tools, " +
			"or use a glibc base such as debian-slim."
	case !musl && !glibc:
		return "no C library found in this base. Dynamically linked tools will not " +
			"run in it; only static binaries will."
	}
	return ""
}

// defaultGuestBaseName is the name `mgit sandbox base` registers under, and
// the one a launch boots when no --image was given.
const defaultGuestBaseName = "base"

// repoGuestBaseRef returns the digest-pinned reference of this repo's guest
// base, or an error naming the one command that creates one.
//
// A digest is not something a person chooses — it is the output of composing
// a base. Requiring --image on every launch would mean copying a 64-hex string
// between commands, where a typo is indistinguishable from tampering. So a
// registered base is used automatically, and its ABSENCE fails closed: mgit
// ships no default base (we redistribute no kernel and no userspace), and
// guessing one would silently boot something the user never chose.
// A launch is also where a repository still carrying an in-tree base from an
// older mgit gets it moved out — the first sandbox command a user runs after
// upgrading, and the last moment before those bytes matter again.
// Refs: MGIT-61.15, FR-17.17, MGIT-147
func repoGuestBaseRef(out io.Writer) (string, error) {
	hostRoot, err := sandboxHostRoot()
	if err != nil {
		return "", err
	}
	if cache, cacheErr := openBaseCache(); cacheErr == nil {
		// Best-effort: a repo that cannot be migrated must still be able to
		// boot the base it has, which is still resolvable by its pinned path.
		if migErr := migrateInTreeBase(hostRoot, cache, out); migErr != nil {
			_, _ = fmt.Fprintf(out, "  warning: %v\n", migErr)
		}
	}
	ref, err := images.PinnedRef(hostRoot, defaultGuestBaseName)
	if errors.Is(err, images.ErrNoSuchImage) {
		return "", fmt.Errorf(
			"this repo has no guest base, so there is nothing to boot.\n\n" +
				"Compose one from any Linux image — mgit pulls it, and ships no " +
				"userspace of its own:\n" +
				"  mgit sandbox base from debian:12\n\n" +
				"Pick an image that already carries your task's toolchain " +
				"(node:22, python:3.12, golang:1.23), or pass an explicitly " +
				"pinned --image <name>@sha256:<hex>")
	}
	if err != nil {
		return "", fmt.Errorf("guest base: %w", err)
	}
	return ref, nil
}
