package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// DaemonVMM is what running this host's sandbox daemon binary with --vmm
// reported, and which binary was asked.
type DaemonVMM struct {
	Path   string
	Report model.VMMReport
}

// DaemonVMMCheck reports which VMM this host's daemon links, where its
// libraries resolved, and whether it can boot a guest at all.
//
// daemon/loads answers "does the binary start"; this answers "can it boot".
// They differ for libkrun: the daemon links libkrun at load time but libkrun
// loads libkrunfw — the library carrying the guest kernel — lazily, by name,
// when a VM context is made. A libkrunfw missing from the Linux release's
// bundled lib/ directory (or from Homebrew) passes daemon/loads, answers
// --version, and fails every launch. Refs: MGIT-229, MGIT-206
type DaemonVMMCheck struct {
	// Probe locates the daemon binary and runs it with --vmm. An error means
	// it could not be asked (no binary, an older build, one that cannot start).
	Probe func(ctx context.Context) (DaemonVMM, error)
	// GOOS picks the remedy's wording: the platforms install differently.
	GOOS string
}

// Name implements Check.
func (DaemonVMMCheck) Name() string { return "daemon/vmm" }

// Run implements Check.
func (c DaemonVMMCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-229"}
	v, err := c.Probe(ctx)
	if err != nil {
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not ask this host's sandbox daemon which VMM it links and whether it can boot a guest"
		return r
	}
	if v.Report.CanBoot() {
		r.Status = StatusOK
		r.Summary = fmt.Sprintf("%s links %s%s", v.Path, v.Report.VMM, librariesPhrase(v.Report.Libraries))
		return r
	}
	r.Status = StatusFailed
	r.Summary = fmt.Sprintf("%s links %s but no guest can boot: %s", v.Path, orUnnamed(v.Report.VMM),
		strings.Join(v.Report.Problems, "; "))
	r.Remedy = vmmRemedy(v.Report.VMM, c.GOOS)
	return r
}

// librariesPhrase lists where each library resolved, for the ok row.
func librariesPhrase(libs []model.VMMLibrary) string {
	if len(libs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(libs))
	for _, l := range libs {
		parts = append(parts, l.Name+" "+l.Path)
	}
	return "; " + strings.Join(parts, ", ")
}

// orUnnamed states an empty VMM name instead of printing a blank.
func orUnnamed(vmm string) string {
	if vmm == "" {
		return "no named VMM"
	}
	return vmm
}

// vmmRemedy is what to do when the linked VMM cannot boot, per backend and
// platform. Linux's libkrun is bundled with the release, so the fix there is
// the archive, never a package manager. Refs: MGIT-229
func vmmRemedy(vmm, goos string) string {
	switch {
	case vmm == model.BackendLibkrun && goos == "darwin":
		return "The macOS release ships libkrun in lib/ beside mgit-sandboxd (install.sh and Homebrew put " +
			"it in <prefix>/lib/mgit); if it is missing, reinstall from the release archive. libkrunfw, the " +
			"guest kernel library, comes from Homebrew (it ships as a dependency of the libkrun formula): " +
			"`brew tap libkrun/krun && brew trust libkrun/krun && brew install libkrun`; " +
			"details: docs/INSTALL-SANDBOX.md"
	case vmm == model.BackendLibkrun:
		return "The Linux release ships libkrun and libkrunfw in lib/ beside mgit-sandboxd " +
			"(install.sh puts them in <prefix>/lib/mgit). Reinstall from the release archive, and keep " +
			"mgit-sandboxd where it can reach that directory; details: docs/INSTALL-SANDBOX.md"
	case vmm == model.BackendKVM:
		return "This daemon is the firecracker build: it needs the firecracker binary on PATH and /dev/kvm " +
			"read-writable by this user. The release archive's daemon uses libkrun instead; " +
			"details: docs/INSTALL-SANDBOX.md"
	default:
		return "Run `mgit-sandboxd --vmm` by hand and read its words; details: docs/INSTALL-SANDBOX.md"
	}
}
