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
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// sealLimit bounds the in-guest seal step.
const sealLimit = 30 * time.Second

// MaxRun is the longest a review may run, and the limit when none is given.
const MaxRun = 30 * time.Minute

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
	// Egress is the host egress stack the guest's network goes through. Nil
	// (the default) launches the guest with no network at all. When it is set
	// the guest launches in allowlist mode with an empty list, so it can reach
	// nothing until a grant is made on this stack. Refs: MGIT-344
	Egress EgressControl
}

// EgressControl is a backend's host egress stack: the proxy and DNS a guest's
// allowlist-mode network is steered to, and the grants that widen it. Every
// method takes the guest's own sandbox id. Refs: MGIT-344, FR-17.8, SEC-05
type EgressControl interface {
	// StartEgress brings the stack up for a launched guest.
	StartEgress(ctx context.Context, info model.SandboxInfo) error
	// StopEgress takes it down; it is safe to call more than once.
	StopEgress(sandboxID string)
	// AllowEgress admits one exact ip:port for the guest.
	AllowEgress(ctx context.Context, sandboxID, entry string) error
	// RevokeAll drops every grant the guest holds.
	RevokeAll(sandboxID string)
}

// Entry is the mapped entrypoint: its digest, its guest path and its argv.
type Entry struct {
	SHA256 string
	Path   string
	Argv   []string
	// Timeout bounds the run; zero means MaxRun.
	Timeout time.Duration
}

// Guest is a provisioned review guest.
type Guest struct {
	ID       string
	LeaseDir string
	uid, gid int
	mgr      model.SandboxManager
	egress   EgressControl
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
	network := model.NetworkPolicy{Mode: model.NetworkModeNone}
	if s.Egress != nil {
		// Default-deny: allowlist mode with nothing on the list. Refs: MGIT-344
		network = model.NetworkPolicy{Mode: model.NetworkModeAllowlist}
	}
	info, err := mgr.Launch(ctx, model.SandboxLaunchOptions{
		TaskID:       s.TaskID,
		WorktreePath: s.Lease.dir,
		ImageRef:     s.Lease.image,
		Network:      network,
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
	g := &Guest{ID: info.ID, LeaseDir: s.Lease.dir, uid: s.UID, gid: s.GID, mgr: mgr}
	if s.Egress != nil {
		// The host stack is up before anything runs in the guest, the seal
		// included; a stack that will not start refuses the run. The mode is
		// stated here rather than read back from the manager, so the stack is
		// always started for the empty allowlist this launch asked for.
		up := *info
		up.NetworkMode, up.NetworkAllowlist = model.NetworkModeAllowlist, nil
		if err := s.Egress.StartEgress(ctx, up); err != nil {
			s.Egress.StopEgress(info.ID)
			_ = mgr.Remove(ctx, info.ID, true)
			return nil, refuse(reviewprep.CodePolicyUnenforcable, "the host network controls for the review guest could not be started")
		}
		g.egress = s.Egress
	}
	// Hand the lease to the reviewer's group, root-owned: the reviewer reads
	// what it was given and owns none of it. Fail closed on any trouble.
	seal, err := mgr.Exec(ctx, info.ID, model.ExecRequest{
		Command: []string{guestBinary, "review-seal", "--gid", strconv.Itoa(s.GID), "--evidence-sha256", s.Lease.att.BundleSHA256, s.Lease.dir},
		AsRoot:  true, Timeout: sealLimit,
	})
	if err != nil || seal.ExitCode != 0 {
		_ = g.Close(ctx)
		detail := "the lease could not be sealed in the guest"
		if err == nil {
			detail += ": " + clip(seal.Stderr, 300) // the seal's own words; they name no bundle content
		}
		return nil, refuse(reviewprep.CodePolicyUnenforcable, detail)
	}
	return g, nil
}

// HasEgress reports whether the guest was launched with a host egress stack.
func (g *Guest) HasEgress() bool { return g.egress != nil }

// AllowEgress admits one exact ip:port for this guest on its own stack, before
// the reviewer starts. A guest with no stack, or a grant the stack refuses, is
// a refusal: the review does not run without what its binding needs.
// Refs: MGIT-344, SEC-05
func (g *Guest) AllowEgress(ctx context.Context, entry string) error {
	if g.egress == nil {
		return refuse(reviewprep.CodePolicyUnenforcable, "the review guest has no network, and a grant was asked for")
	}
	if err := g.egress.AllowEgress(ctx, g.ID, entry); err != nil {
		return refuse(reviewprep.CodePolicyUnenforcable, "the host did not admit the network grant "+clip([]byte(entry), 80))
	}
	return nil
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
	limit := e.Timeout
	if limit == 0 {
		limit = MaxRun
	}
	if limit < 0 || limit > MaxRun {
		return nil, refuse(reviewprep.CodeMappingMismatch, "the run limit is outside the allowed range")
	}
	nonce, err := newNonce()
	if err != nil {
		return nil, fmt.Errorf("review guest nonce: %w", err)
	}
	// The limit is enforced here, at the limit: the exec's deadline is the limit
	// itself (the microvm backend puts it on the guest connection), and a result
	// that arrives after it is a timeout even if the backend let it through. The
	// caller removes the guest afterwards, which ends a reviewer still running.
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	res, err := g.mgr.Exec(runCtx, g.ID, model.ExecRequest{
		Command: cmd, Dir: g.LeaseDir, AsRoot: true, Timeout: limit, Env: []string{NonceEnv + "=" + nonce},
	})
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("review guest exec: %w", cerr) // the caller went away; not the run's own timeout
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("review guest exec: the run passed its limit of %v: %w", limit, context.DeadlineExceeded)
	}
	if err != nil {
		return nil, fmt.Errorf("review guest exec: %w", err)
	}
	if res.ExitCode == launcherRefusalExit {
		if env, ok := launcherRefusal(res.Stderr, nonce); ok {
			return res, &env
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
func launcherRefusal(stderr []byte, nonce string) (reviewprep.Error, bool) {
	var env struct {
		reviewprep.Error
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stderr), &env); err != nil {
		return reviewprep.Error{}, false
	}
	if env.Schema != reviewprep.ErrorSchema || env.Code == "" || env.Nonce != nonce {
		return reviewprep.Error{}, false
	}
	return env.Error, true
}

// Close takes down the guest's egress stack, if it has one, and removes the guest.
func (g *Guest) Close(ctx context.Context) error {
	if g.egress != nil {
		// No grant outlives the guest, and the stack goes before the guest.
		g.egress.RevokeAll(g.ID)
		g.egress.StopEgress(g.ID)
	}
	return g.mgr.Remove(ctx, g.ID, true)
}

// clip returns the first n bytes of b as one trimmed line.
func clip(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return strings.Join(strings.Fields(string(b)), " ")
}
