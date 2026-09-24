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
//
// The echo verb exists only for that check, so a refusal of an echo that
// ASKED for more than the cap is logged as the probe it is (writeResponseAs),
// not as a write failure. An echo at or under the cap must arrive; if one
// is refused for size, the cap arithmetic is wrong, and that still warns.
// Refs: MGIT-235
func (d *Daemon) serveEcho(conn net.Conn, args *controlproto.EchoArgs) {
	resp, err := controlproto.BuildEchoResponse(args.Bytes)
	if err != nil {
		d.reply(conn, resp, err)
		return
	}
	d.writeResponseAs(conn, resp, askedOverTheCap(args.Bytes))
}

// askedOverTheCap reports whether an echo asked for more than the control
// response cap: the one case whose refusal is the probe working.
func askedOverTheCap(bytes int) bool { return bytes > controlproto.MaxResponseBytes }
