package doctor

import (
	"context"

	"github.com/hyper-swe/mgit/internal/sandboxd/daemonrec"
)

// ServingDaemonVersionCheck compares the daemon serving this repository with
// this CLI. RED: the comparison is not written yet. Refs: MGIT-221
type ServingDaemonVersionCheck struct {
	List     func(ctx context.Context) ([]daemonrec.Listed, error)
	RepoRoot func() (string, error)
	CLI      string
}

// Name implements Check.
func (ServingDaemonVersionCheck) Name() string { return "daemon/serving-version" }

// Run implements Check.
func (c ServingDaemonVersionCheck) Run(_ context.Context) Result {
	return Result{Name: c.Name(), Incident: "MGIT-221", Status: StatusOK}
}
