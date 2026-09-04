package sandboxd

import (
	"context"
	"fmt"
	"net"

	"github.com/hyper-swe/mgit/internal/controlproto"
	"github.com/hyper-swe/mgit/internal/model"
)

// VerifyGuestView asks the daemon whether a task's guest reads what was last
// delivered to it. The daemon owns the delivered manifest and the exec
// channel, so the question is asked of it rather than answered from its
// state directory. Refs: MGIT-164, MGIT-154
func (c *Client) VerifyGuestView(ctx context.Context, taskID string) (*model.GuestViewReport, error) {
	resp, err := c.roundTripRaw(ctx, &controlproto.Request{
		Kind: controlproto.KindSyncVerify, SyncVerify: &controlproto.TaskRef{TaskID: taskID},
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("sandbox: %s", resp.Error)
	}
	if resp.SyncVerify == nil || resp.SyncVerify.View == nil {
		return nil, fmt.Errorf("sandbox: the daemon answered the sync-verify request without a view")
	}
	return resp.SyncVerify.View, nil
}

// serveSyncVerify answers a sync-verify request from the service, when the
// service can ask a guest; otherwise it refuses in the operator's words.
// Refs: MGIT-164, MGIT-171
func (d *Daemon) serveSyncVerify(ctx context.Context, conn net.Conn, ref *controlproto.TaskRef) {
	verifier, ok := d.cfg.Service.(model.GuestViewVerifier)
	if d.cfg.Service == nil || !ok {
		d.reply(conn, &controlproto.Response{}, d.unservedVerb(ctx, ref.TaskID, "doctor (guest/delivery)",
			"this daemon cannot ask a guest what it reads for the paths delivered to it",
			"run a daemon built with the sandbox service, or re-deliver with `mgit sandbox sync`, which "+
				"confirms from inside the guest on its own",
			"nothing was asked of the guest"))
		return
	}
	view, err := verifier.VerifyGuestView(ctx, ref.TaskID)
	d.reply(conn, &controlproto.Response{SyncVerify: &controlproto.SyncVerifyResult{View: view}}, err)
}
