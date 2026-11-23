package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/reviewprep"
)

// ReviewPreparer serves the review-prepare verb (MGIT-333): it checks a review
// request against the operator's binding map and the bundle, seals the bundle,
// and returns the signed attestation as JSON. A refusal is a
// *reviewprep.Error. It starts nothing.
type ReviewPreparer interface {
	PrepareReview(ctx context.Context, args controlproto.ReviewPrepareArgs) ([]byte, error)
}

// ReviewPrepare asks the daemon for a signed review attestation. A refusal
// comes back as a *reviewprep.Error, with reviewer_started=false.
func (c *Client) ReviewPrepare(ctx context.Context, args controlproto.ReviewPrepareArgs) ([]byte, error) {
	resp, err := c.roundTripRaw(ctx, &controlproto.Request{Kind: controlproto.KindReviewPrepare, ReviewPrepare: &args})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("sandbox: %s", resp.Error)
	}
	if resp.ReviewPrepare == nil {
		return nil, fmt.Errorf("sandbox: the daemon answered the review request without a result")
	}
	if len(resp.ReviewPrepare.Refusal) > 0 {
		var e reviewprep.Error
		if err := json.Unmarshal(resp.ReviewPrepare.Refusal, &e); err != nil || e.Code == "" {
			return nil, fmt.Errorf("sandbox: the daemon refused the review request in a form this client cannot read")
		}
		return nil, &e
	}
	if len(resp.ReviewPrepare.Attestation) == 0 {
		return nil, fmt.Errorf("sandbox: the daemon answered the review request without an attestation")
	}
	return resp.ReviewPrepare.Attestation, nil
}

// serveReviewPrepare routes one review-prepare request. A refusal travels as
// its own envelope, not as a transport error. Refs: MGIT-333
func (d *Daemon) serveReviewPrepare(ctx context.Context, conn net.Conn, args *controlproto.ReviewPrepareArgs) {
	if d.cfg.ReviewPreparer == nil {
		d.reply(conn, &controlproto.Response{}, d.unservedVerb(ctx, "", "review prepare",
			"no review service is wired, so no review request can be checked or attested",
			"run a daemon built with the review service",
			"no bundle was read and no attestation was issued"))
		return
	}
	att, err := d.cfg.ReviewPreparer.PrepareReview(ctx, *args)
	var refusal *reviewprep.Error
	if errors.As(err, &refusal) {
		raw, _ := json.Marshal(refusal)
		d.reply(conn, &controlproto.Response{ReviewPrepare: &controlproto.ReviewPrepareResult{Refusal: raw}}, nil)
		return
	}
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	d.reply(conn, &controlproto.Response{ReviewPrepare: &controlproto.ReviewPrepareResult{Attestation: att}}, nil)
}
