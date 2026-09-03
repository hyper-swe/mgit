package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/doctor"
	"github.com/hyper-swe/mgit/internal/guestnet"
	"github.com/hyper-swe/mgit/internal/model"
	"github.com/hyper-swe/mgit/internal/sandboxd"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// doctorCmd runs the standing checks for conditions that have already cost
// someone a diagnosis.
//
// Each check converts one closed incident, per R-H300 rule 5 — an incident is
// not finished until the thing that went wrong becomes a thing the tool asks
// itself. The output names the incident behind each check, so the reason it
// exists outlives the people who remember it. Refs: MGIT-162, R-H300
func doctorCmd(connect connectFunc) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check this repository and its sandbox for known-bad conditions",
		Long: "Runs cheap standing checks for conditions that have already broken something " +
			"for someone. A check reports ok, failed, or not-checked — and a not-checked " +
			"check is an absence of evidence, never a pass.",
		Args: cobra.NoArgs,
		// The non-zero exit is the verdict; the report above it is the
		// message. Letting cobra also print "Error: exit status 1" adds a
		// line that names nothing and reads like a malfunction of doctor
		// rather than a finding by it. Refs: MGIT-162
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openAppFromCwd()
			if err != nil {
				return err
			}
			defer app.Close()

			results := doctor.Run(cmd.Context(), doctorChecks(app, connect))
			if asJSON {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(
					map[string]any{"checks": results}); err != nil {
					return fmt.Errorf("encode doctor report: %w", err)
				}
			} else {
				_, _ = fmt.Fprint(cmd.OutOrStdout(), doctor.Render(results))
			}
			if doctor.Failed(results) {
				// Exit non-zero on a FAILED check only. A check that could not
				// run must never fail the exit code, or readers learn to
				// ignore it — which is how a gate stops being consulted.
				return &exitError{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
}

// doctorChecks assembles the checks with their real probes.
func doctorChecks(app *App, connect connectFunc) []doctor.Check {
	return []doctor.Check{
		doctor.NestedGitCheck{Scan: func() ([]string, error) {
			return gitstore.NewWorktreeStore(app.Repo).RecordedNestedRepos(context.Background())
		}},
		doctor.GuestLocalhostCheck{Probe: func(ctx context.Context) (string, error) {
			return probeGuestLocalhost(ctx, connect, app.BoundTask)
		}},
		doctor.BaseCurrencyCheck{Inspect: inspectBaseCurrency},
		doctor.ResponseCapCheck{Probe: func(ctx context.Context, bytes int) (doctor.EchoReply, error) {
			return probeResponseCap(ctx, connect, bytes)
		}},
		doctor.GuestSyncVerifyCheck{Probe: func(ctx context.Context) (string, error) {
			return probeGuestSyncVerify(ctx, connect, app.BoundTask)
		}},
	}
}

// probeGuestSyncVerify asks the task's guest which of the two tools a sync's
// guest-side confirmation relies on it has: sha256sum, to read a delivered
// digest with, and a writable /proc/sys/vm/drop_caches, to invalidate its
// cached view with. Each present tool is printed on its own line; the check
// reads the lines. Refs: MGIT-192
func probeGuestSyncVerify(ctx context.Context, connect connectFunc, taskID string) (string, error) {
	if taskID == "" {
		return "", errors.New("no sandbox: this directory is not bound to a task worktree")
	}
	cl, err := connect(ctx)
	if err != nil {
		return "", fmt.Errorf("no sandbox daemon reachable: %w", err)
	}
	const script = "command -v sha256sum >/dev/null 2>&1 && echo sha256sum; " +
		"[ -w /proc/sys/vm/drop_caches ] && echo drop_caches; exit 0"
	var out, errOut strings.Builder
	code, err := cl.Exec(ctx, taskID, model.ExecRequest{Command: []string{"sh", "-c", script}}, &out, &errOut)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("the guest's shell exited %d: %s", code, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// echoer is the one verb the response-cap probe needs. It is asserted on the
// connected client rather than added to sandboxClient, so a client without
// it reports that it cannot ask — a not-checked, never a pass. Refs: MGIT-175
type echoer interface {
	Echo(ctx context.Context, bytes int) (*sandboxd.EchoOutcome, error)
}

// probeResponseCap asks the daemon for a control response of exactly bytes
// and reports what came back: intact, refused (the daemon's words), or
// arrived-but-wrong (how). Anything that stops it asking at all is an error,
// so the check reports not-checked with the reason. Refs: MGIT-175, MGIT-160
func probeResponseCap(ctx context.Context, connect connectFunc, bytes int) (doctor.EchoReply, error) {
	cl, err := connect(ctx)
	if err != nil {
		return doctor.EchoReply{}, fmt.Errorf("no sandbox daemon reachable: %w", err)
	}
	e, ok := cl.(echoer)
	if !ok {
		return doctor.EchoReply{}, errors.New("this client cannot ask the daemon to echo")
	}
	out, err := e.Echo(ctx, bytes)
	if err != nil {
		return doctor.EchoReply{}, err
	}
	reply := doctor.EchoReply{Requested: bytes}
	if out.Refusal != "" {
		reply.Refusal = out.Refusal
		return reply, nil
	}
	if err := controlproto.VerifyEcho(out.Result); err != nil {
		reply.Detail = "the answer did not verify: " + err.Error()
		return reply, nil //nolint:nilerr // a failed verification is the FINDING, carried as data; only "could not ask" is an error
	}
	if out.Result.Bytes != bytes {
		reply.Detail = fmt.Sprintf("the answer is %d bytes, not the %d asked for", out.Result.Bytes, bytes)
		return reply, nil
	}
	reply.Intact = true
	return reply, nil
}

// inspectBaseCurrency reports which mgit composed this repository's guest base
// and which one is running now.
//
// The composing version is read from the base's own tree, where the content
// digest already covers it — so the answer cannot be forged without breaking
// the pin that boots it. A base with no marker returns "" and is reported as
// unknown, which the check treats as a failure rather than a pass.
// Refs: MGIT-174
func inspectBaseCurrency() (composed, running string, err error) {
	hostRoot, err := sandboxHostRoot()
	if err != nil {
		return "", "", fmt.Errorf("no sandbox host root for this repository: %w", err)
	}
	ref, err := images.PinnedRef(hostRoot, defaultGuestBaseName)
	if err != nil {
		return "", "", fmt.Errorf("no guest base registered for this repository: %w", err)
	}
	cache, cacheErr := openBaseCache()
	if cacheErr != nil {
		return "", "", fmt.Errorf("could not open the base cache: %w", cacheErr)
	}
	store, err := images.NewStoreWithBaseCache(hostRoot, func() time.Time { return time.Now().UTC() }, cache)
	if err != nil {
		return "", "", fmt.Errorf("could not open the image store: %w", err)
	}
	resolved, err := store.Resolve(ref)
	if err != nil {
		return "", "", fmt.Errorf("could not resolve the pinned guest base: %w", err)
	}
	rec, readErr := guestbase.ReadComposedBy(resolved.RootfsPath)
	if errors.Is(readErr, guestbase.ErrComposedByUnknown) {
		// Not an inspection failure: we looked and it genuinely says nothing.
		// That distinction matters — "could not look" and "looked and found no
		// record" are different facts, and only the second is a finding.
		return "", Version, nil
	}
	if readErr != nil {
		return "", "", readErr
	}
	return rec.Version, Version, nil
}

// probeGuestLocalhost reads the task guest's own name table and returns the
// line that maps localhost, or "" when nothing does.
//
// It READS /etc/hosts — the file internal/guestnet writes — rather than
// running a resolver. The property under test is "localhost resolves WITHOUT
// a DNS query", and `getent hosts` is a resolver: with mgit's own guest
// nsswitch of `hosts: files dns` it falls through to DNS on a miss, so on a
// sandbox whose egress reaches a resolver it passed a check whose whole point
// is that DNS is unavailable (MGIT-169). Reading the table is the direct
// question.
//
// Exit codes are distinguished, because a check that could not run must not
// read as the failure: exit 0 is an answer (parsed for a localhost entry);
// exit 1 with "No such file" is an answer too — no table at all IS the
// MGIT-159 condition; anything else (127 for a base without `cat`, a
// permission failure) is an error the caller reports as not-checked, with
// the exit code and stderr as the reason. Refs: MGIT-162, MGIT-159, MGIT-169
func probeGuestLocalhost(ctx context.Context, connect connectFunc, taskID string) (string, error) {
	if taskID == "" {
		return "", errors.New("no sandbox: this directory is not bound to a task worktree")
	}
	cl, err := connect(ctx)
	if err != nil {
		return "", fmt.Errorf("no sandbox daemon reachable: %w", err)
	}
	var out, errOut strings.Builder
	code, err := cl.Exec(ctx, taskID,
		model.ExecRequest{Command: []string{"cat", "/etc/hosts"}},
		&out, &errOut)
	if err != nil {
		return "", err
	}
	switch {
	case code == 0:
		return guestnet.LocalhostEntry(out.String()), nil
	case code == 1 && strings.Contains(errOut.String(), "No such file"):
		return "", nil
	default:
		return "", fmt.Errorf("could not read the guest's name table: `cat /etc/hosts` exited %d: %s",
			code, strings.TrimSpace(errOut.String()))
	}
}
