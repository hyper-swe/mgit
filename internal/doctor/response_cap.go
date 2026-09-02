package doctor

import (
	"context"
	"fmt"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// EchoReply is what came back from asking the daemon for a control response
// of a given size. Exactly one of Intact, Refusal and Detail describes it.
type EchoReply struct {
	Requested int    // encoded bytes asked for
	Intact    bool   // the answer arrived and verified byte-for-byte
	Refusal   string // the daemon's own refusal, verbatim, when it declined
	Detail    string // why the answer was not intact, when it arrived but did not verify
}

// ResponseCapCheck reports whether the daemon can answer its largest legal
// control response, and refuses a larger one legibly.
//
// From MGIT-160: a sync classification over a large worktree produced an
// answer over the 1 MiB control-response cap. The daemon refused to send it
// and logged why; the client saw "read response: EOF". Six diagnostic round
// trips and two falsified theories later, the cause was found where the
// daemon had known it all along. The fix made the refusal cross the wire;
// this check asks, on the real channel, that it still does — and that a
// full-size answer still arrives at all.
//
// It is a PROPERTY asked as a STATE: rather than estimate whether some
// future answer would fit, the daemon is asked for one at the cap, now, and
// for one byte over it. Refs: MGIT-175, MGIT-160, R-H300
type ResponseCapCheck struct {
	// Probe asks the daemon for a control response of exactly bytes and
	// reports what came back. An error means it could not ask at all.
	Probe func(ctx context.Context, bytes int) (EchoReply, error)
}

// Name implements Check.
func (ResponseCapCheck) Name() string { return "daemon/response-cap" }

// Run implements Check.
func (c ResponseCapCheck) Run(ctx context.Context) Result {
	r := Result{Name: c.Name(), Incident: "MGIT-160"}
	const cap = controlproto.MaxResponseBytes
	full, err := c.Probe(ctx, cap)
	if err != nil {
		// No daemon, no client verb, a canceled context: reasons the check
		// could not RUN. None of them says the channel is fine.
		r.Status, r.Reason = StatusNotChecked, err.Error()
		r.Summary = "could not ask the daemon for a full-size control response"
		return r
	}
	if !full.Intact {
		observed := full.Detail
		if full.Refusal != "" {
			observed = "the daemon refused it: " + full.Refusal
		}
		r.Status = StatusFailed
		r.Summary = fmt.Sprintf("a full %d-byte control response did not arrive intact — %s. Any verb whose "+
			"answer approaches the cap, such as a sync classification over a large tree, fails the same way", cap, observed)
		r.Remedy = "restart the daemon from this build (`pkill mgit-sandboxd`; the next mgit command starts it again) " +
			"and re-run doctor; if it persists, the daemon log's write_error lines carry the daemon's side"
		return r
	}
	over, err := c.Probe(ctx, cap+1)
	if err != nil {
		r.Status = StatusFailed
		r.Summary = fmt.Sprintf("an oversized control response ended the connection (%v) instead of a refusal — "+
			"the MGIT-160 shape, where the only record of the cause is the daemon's own log", err)
		r.Remedy = "restart the daemon from this build and re-run doctor; a daemon that still answers this way " +
			"is not the build that carries the MGIT-160 refusal"
		return r
	}
	if over.Refusal == "" {
		r.Status = StatusFailed
		r.Summary = "an oversized control response was delivered, so the response cap is not enforced by this daemon"
		r.Remedy = "restart the daemon from this build and re-run doctor"
		return r
	}
	r.Status = StatusOK
	r.Summary = fmt.Sprintf("the daemon answered a full %d-byte control response intact and refused an oversized one legibly", cap)
	return r
}
