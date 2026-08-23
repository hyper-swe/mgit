package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/doctor"
	"github.com/hyper-swe/mgit/internal/model"
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
	}
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

// probeGuestLocalhost resolves localhost inside the task's guest.
//
// It asks the guest to read its own name table rather than to run a resolver:
// the property under test is "localhost resolves WITHOUT a DNS query", and a
// resolver that succeeded via DNS would pass a check whose whole point is that
// DNS is unavailable. Refs: MGIT-162, MGIT-159
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
		model.ExecRequest{Command: []string{"getent", "hosts", "localhost"}},
		&out, &errOut)
	if err != nil {
		return "", err
	}
	if code != 0 {
		// getent exits non-zero when it resolves nothing, which IS the
		// condition — an empty answer, reported as such rather than as an
		// error the caller cannot interpret.
		return "", nil
	}
	return out.String(), nil
}
