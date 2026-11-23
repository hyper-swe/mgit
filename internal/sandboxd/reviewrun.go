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

// ReviewRunner serves the review-run verb (MGIT-333): it re-verifies a prepared
// lease, starts the review guest, runs the operator-mapped program under the
// limits and returns the result as JSON. A refusal is a *reviewprep.Error.
type ReviewRunner interface {
	RunReview(ctx context.Context, args controlproto.ReviewRunArgs) ([]byte, error)
}

// ReviewRun asks the daemon to run a prepared review. A refusal comes back as a
// *reviewprep.Error that says whether the reviewer had started.
func (c *Client) ReviewRun(ctx context.Context, args controlproto.ReviewRunArgs) ([]byte, error) {
	resp, err := c.roundTripRaw(ctx, &controlproto.Request{Kind: controlproto.KindReviewRun, ReviewRun: &args})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("sandbox: %s", resp.Error)
	}
	if resp.ReviewRun == nil {
		return nil, fmt.Errorf("sandbox: the daemon answered the review run without a result")
	}
	if len(resp.ReviewRun.Refusal) > 0 {
		var e reviewprep.Error
		if err := json.Unmarshal(resp.ReviewRun.Refusal, &e); err != nil || e.Code == "" {
			return nil, fmt.Errorf("sandbox: the daemon refused the review run in a form this client cannot read")
		}
		return nil, &e
	}
	if len(resp.ReviewRun.Result) == 0 {
		return nil, fmt.Errorf("sandbox: the daemon answered the review run without a result")
	}
	return resp.ReviewRun.Result, nil
}

// serveReviewRun routes one review-run request. A refusal travels as its own
// envelope, not as a transport error. Refs: MGIT-333
func (d *Daemon) serveReviewRun(ctx context.Context, conn net.Conn, args *controlproto.ReviewRunArgs) {
	if d.cfg.ReviewRunner == nil {
		d.reply(conn, &controlproto.Response{}, d.unservedVerb(ctx, "", "review run",
			"no review guest backend is wired, so no review can be run",
			"run a daemon built with the review service on a launch-time-image backend",
			"nothing was started"))
		return
	}
	res, err := d.cfg.ReviewRunner.RunReview(ctx, *args)
	var refusal *reviewprep.Error
	if errors.As(err, &refusal) {
		raw, _ := json.Marshal(refusal)
		d.reply(conn, &controlproto.Response{ReviewRun: &controlproto.ReviewRunResult{Refusal: raw}}, nil)
		return
	}
	if err != nil {
		d.reply(conn, &controlproto.Response{}, err)
		return
	}
	d.reply(conn, &controlproto.Response{ReviewRun: &controlproto.ReviewRunResult{Result: res}}, nil)
}
