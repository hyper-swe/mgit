// Package reviewguest provisions the guest the reviewer-ro/v1 profile runs in,
// and runs the pinned entrypoint in it through the launcher.
//
// The host-side contract is small on purpose. The sealed lease directory is
// the only host path that reaches the guest, it is delivered as a launch-time
// image (so nothing the guest writes can travel back to the host), the guest
// has no network and no repository, and the entrypoint is only ever started by
// `mgit-guest review-exec`, which checks its digest on the file it executes.
// Refs: FEAT-3.347.1, MGIT-333
package reviewguest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/reviewlaunch"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// launchTimeBackend is the one backend that delivers the worktree as a block
// image built at launch. The others (libkrun, vzf, container) share the host
// directory live, so a write in the guest could reach the lease, and hyperv's
// delivery is not shown to be a copy; each is refused rather than assumed.
const launchTimeBackend = model.BackendKVM

// guestBinary is where the guest image installs the launcher.
const guestBinary = "/sbin/mgit-guest"

// launcherRefusalExit is the status review-exec exits with when it refuses.
const launcherRefusalExit = 70

// NonceEnv is the variable that carries the per-run refusal nonce to the launcher.
const NonceEnv = reviewlaunch.RefusalNonceEnv

// The review guest's size is fixed: the hypervisor refuses a launch with no
// CPU or memory, and the profile does not take them from the caller, so every
// review guest is the same shape. 256 MiB holds the image and one reviewer.
const (
	ReviewCPUs     = 1
	ReviewMemoryMB = 256
)

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Spec is what Provision needs.
type Spec struct {
	// Backend is the backend the manager reports; it must be launch-time.
	Backend string
	// Lease is the sealed, verified lease the guest is built from; the image
	// is the one its attestation was issued for.
	Lease  *Lease
	TaskID string
	// UID and GID are the unprivileged identity the reviewer runs as.
	UID, GID int
}

// Entry is the mapped entrypoint: its digest, its guest path and its argv.
type Entry struct {
	SHA256 string
	Path   string
	Argv   []string
}

// Guest is a provisioned review guest.
type Guest struct {
	ID       string
	LeaseDir string
	uid, gid int
	mgr      model.SandboxManager
}

func refuse(code, detail string) *reviewprep.Error {
	return &reviewprep.Error{Schema: reviewprep.ErrorSchema, Code: code, Phase: "prepare", Detail: detail}
}

// Provision launches the review guest on the lease directory, or refuses.
func Provision(ctx context.Context, mgr model.SandboxManager, s Spec) (*Guest, error) {
	if s.Backend != launchTimeBackend {
		return nil, refuse(reviewprep.CodePolicyUnenforcable,
			fmt.Sprintf("backend %q does not deliver the bundle as a launch-time image", s.Backend))
	}
	if s.UID <= 0 || s.GID <= 0 {
		return nil, refuse(reviewprep.CodeMappingMismatch, "the reviewer identity must be an unprivileged uid and gid")
	}
	if s.Lease == nil || s.Lease.dir == "" {
		return nil, refuse(reviewprep.CodeBundleInvalid, "a sealed lease is required")
	}
	if err := model.ValidateImageRef(s.Lease.image); err != nil {
		return nil, refuse(reviewprep.CodeImageMismatch, "the image is not pinned by digest")
	}
	info, err := mgr.Launch(ctx, model.SandboxLaunchOptions{
		TaskID:       s.TaskID,
		WorktreePath: s.Lease.dir,
		ImageRef:     s.Lease.image,
		Network:      model.NetworkPolicy{Mode: model.NetworkModeNone},
		NoGuestRepo:  true,
		CPUs:         ReviewCPUs,
		MemoryMB:     ReviewMemoryMB,
	})
	if err != nil {
		return nil, fmt.Errorf("review guest launch: %w", err)
	}
	if info.Backend != launchTimeBackend {
		// The manager is not the backend the caller declared; do not run a
		// reviewer on a live share.
		_ = mgr.Remove(ctx, info.ID, true)
		return nil, refuse(reviewprep.CodePolicyUnenforcable,
			fmt.Sprintf("the launched backend %q is not a launch-time image", info.Backend))
	}
	return &Guest{ID: info.ID, LeaseDir: s.Lease.dir, uid: s.UID, gid: s.GID, mgr: mgr}, nil
}

// Run starts the entrypoint through the launcher. A launcher refusal (exit 70
// with the envelope on stderr) comes back as a *reviewprep.Error; any other
// exit status is the reviewer's own.
func (g *Guest) Run(ctx context.Context, e Entry) (*model.ExecResult, error) {
	if !hexDigest.MatchString(e.SHA256) || !filepath.IsAbs(e.Path) {
		return nil, refuse(reviewprep.CodeMappingMismatch, "the entrypoint needs a sha256 digest and an absolute path")
	}
	cmd := make([]string, 0, 11+len(e.Argv))
	cmd = append(cmd, guestBinary, "review-exec", "--sha256", e.SHA256,
		"--uid", strconv.Itoa(g.uid), "--gid", strconv.Itoa(g.gid), "--dir", g.LeaseDir, "--", e.Path)
	cmd = append(cmd, e.Argv...)
	nonce, err := newNonce()
	if err != nil {
		return nil, fmt.Errorf("review guest nonce: %w", err)
	}
	res, err := g.mgr.Exec(ctx, g.ID, model.ExecRequest{
		Command: cmd, Dir: g.LeaseDir, AsRoot: true, Env: []string{NonceEnv + "=" + nonce},
	})
	if err != nil {
		return nil, fmt.Errorf("review guest exec: %w", err)
	}
	if res.ExitCode == launcherRefusalExit {
		if env := launcherRefusal(res.Stderr, nonce); env != nil {
			return res, env
		}
	}
	return res, nil
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// launcherRefusal returns the refusal the launcher printed, or nil. The
// reviewed program shares the launcher's stderr, so an envelope counts only if
// it carries this run's nonce, which only the launcher is given.
func launcherRefusal(stderr []byte, nonce string) *reviewprep.Error {
	var env struct {
		reviewprep.Error
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(bytes.TrimSpace(stderr), &env) != nil {
		return nil
	}
	if env.Schema != reviewprep.ErrorSchema || env.Code == "" || env.Nonce != nonce {
		return nil
	}
	e := env.Error
	return &e
}

// Close removes the guest.
func (g *Guest) Close(ctx context.Context) error { return g.mgr.Remove(ctx, g.ID, true) }
