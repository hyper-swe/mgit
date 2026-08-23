package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
)

// NestedGitCheck reports whether this repository's recorded tree contains a
// nested .git.
//
// From MGIT-157: a .git below the repo root was absorbed into the base, and
// the damage then surfaced somewhere else entirely — every later `mgit work`
// died inside go-git's tree walker. The condition is detectable AT REST, which
// is the whole reason it is worth a check: the incident was only ever noticed
// when something failed. Refs: MGIT-162, MGIT-157
type NestedGitCheck struct {
	// Scan returns the nested-repository paths recorded in the tree.
	Scan func() ([]string, error)
}

// Name implements Check.
func (NestedGitCheck) Name() string { return "tree/nested-git" }

// Run implements Check.
func (c NestedGitCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-157"}
	offenders, err := c.Scan()
	if err != nil {
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not read this repository's recorded tree"
		return r
	}
	if len(offenders) == 0 {
		r.Status = StatusOK
		r.Summary = "no nested repository is recorded in the tree"
		return r
	}
	r.Status = StatusFailed
	r.Summary = fmt.Sprintf("%d nested repository path(s) recorded in the tree, starting with %s — "+
		"every `mgit work` on this repository will fail until it is re-recorded",
		len(offenders), offenders[0])
	r.Remedy = "move `.mgit` aside and run `mgit init` to record a clean base. The nested " +
		"repository can stay where it is — mgit now skips it, and your own history is untouched"
	return r
}

// GuestLocalhostCheck reports whether a task's guest can resolve localhost
// WITHOUT asking the network.
//
// From MGIT-159: the guest shipped no /etc/hosts, so every localhost lookup
// fell through to DNS and died under the default deny-all egress — taking
// vitest, vite, jest and anything else binding a local port with it. The error
// was EAI_AGAIN, which points at the network policy and sends the reader
// somewhere else entirely, so the condition is worth asking about directly.
// Refs: MGIT-162, MGIT-159
type GuestLocalhostCheck struct {
	// Probe resolves localhost inside the guest and returns what it found.
	Probe func(ctx context.Context) (string, error)
}

// Name implements Check.
func (GuestLocalhostCheck) Name() string { return "guest/localhost" }

// Run implements Check.
func (c GuestLocalhostCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-159"}
	out, err := c.Probe(ctx)
	if err != nil {
		// No sandbox, no daemon, a backend that cannot exec: all reasons the
		// check could not RUN. None of them is evidence that the guest is fine.
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not ask a guest whether it resolves localhost"
		return r
	}
	if strings.Contains(out, "localhost") {
		r.Status = StatusOK
		r.Summary = "the guest resolves localhost from its own name table, with no DNS query"
		return r
	}
	r.Status = StatusFailed
	r.Summary = "the guest cannot resolve localhost without DNS, so anything binding or dialing " +
		"a local port fails with EAI_AGAIN under the default deny-all egress — which reads as a " +
		"network-policy problem and is not one"
	r.Remedy = "recompose this repository's guest base with `mgit sandbox base from <image>`; " +
		"bases composed before mgit wrote a static name table carry no /etc/hosts"
	return r
}

// BaseCurrencyCheck reports whether this repository's guest base was composed
// by the substrate now running it.
//
// From MGIT-174: nothing compared the two, because nothing recorded the
// composing substrate — so a base composed under an older mgit produced no
// warning for two consecutive releases, and a human hand-refreshed it twice.
// R-H300 rule 5 says that is doctor's job, not a person's.
//
// It matters because the guest binaries are injected AT COMPOSE TIME and
// frozen there: a stale base silently runs older guest code and so silently
// lacks the guest-side fixes of every release since it was composed — while
// the release notes say those fixes shipped. Refs: MGIT-162, MGIT-174
type BaseCurrencyCheck struct {
	// Inspect reports the base's composing version and the running one.
	Inspect func() (composed, running string, err error)
}

// Name implements Check.
func (BaseCurrencyCheck) Name() string { return "base/currency" }

// Run implements Check.
func (c BaseCurrencyCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-174"}
	composed, running, err := c.Inspect()
	if err != nil {
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not determine which mgit composed this repository's guest base"
		return r
	}
	switch guestbase.BaseCurrency(composed, running) {
	case guestbase.CurrencyCurrent:
		r.Status = StatusOK
		r.Summary = fmt.Sprintf("the guest base was composed by this substrate (%s)", running)
	case guestbase.CurrencyUnknown:
		// Deliberately NOT ok. Reporting silence as currency is the exact
		// failure being fixed: for two releases the absence of a warning was
		// read as an assurance.
		r.Status = StatusFailed
		r.Summary = "this guest base does not record what composed it, so whether its guest " +
			"binaries match this substrate cannot be established"
		r.Remedy = "recompose it with `mgit sandbox base from <image>`; bases composed before " +
			"mgit recorded this carry no marker"
	default:
		r.Status = StatusFailed
		r.Summary = fmt.Sprintf("the guest base was composed by mgit %s but this substrate is %s, "+
			"so the guest binaries frozen into it are not this build's — it silently lacks every "+
			"guest-side fix since %s", composed, running, composed)
		r.Remedy = "recompose it with `mgit sandbox base from <image>`"
	}
	return r
}
