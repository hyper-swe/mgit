package sandboxd

import (
	"context"
	"net"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// EchoOutcome is what came back from asking the daemon to echo: the answer,
// or the daemon's own refusal. A refusal is a RESPONSE — it crossed the wire
// as one — which is precisely the distinction MGIT-160's doctor check exists
// to draw; only a transport failure is an error. Refs: MGIT-175, MGIT-160
type EchoOutcome struct {
	Result  *controlproto.EchoResult
	Refusal string
}

// Echo asks the daemon for a control response of exactly bytes.
//
// It uses the raw round trip because the daemon's refusal text is the datum:
// an over-cap request is SUPPOSED to be refused, and the check needs to see
// that the refusal arrived as a response rather than as a closed socket.
// Refs: MGIT-175, MGIT-160
func (c *Client) Echo(ctx context.Context, bytes int) (*EchoOutcome, error) {
	resp, err := c.roundTripRaw(ctx, &controlproto.Request{
		Kind: controlproto.KindEcho, Echo: &controlproto.EchoArgs{Bytes: bytes},
	})
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return &EchoOutcome{Refusal: resp.Error}, nil
	}
	return &EchoOutcome{Result: resp.Echo}, nil
}

// serveEcho answers an echo. An over-cap answer is BUILT and handed to reply,
// where WriteResponse refuses it and the MGIT-160 path sends the small
// refusal in its place — the mechanism the check provokes, exercised rather
// than simulated. Refs: MGIT-175, MGIT-160
func (d *Daemon) serveEcho(conn net.Conn, args *controlproto.EchoArgs) {
	resp, err := controlproto.BuildEchoResponse(args.Bytes)
	d.reply(conn, resp, err)
}
