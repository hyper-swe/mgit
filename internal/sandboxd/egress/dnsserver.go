package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsAnswerTTL is the TTL on returned records. It is deliberately short so
// the guest re-queries often: resolution stays pinned-fresh and every query
// passes back through the rate-limited, allowlist-gated resolver (SEC-07).
const dnsAnswerTTL = 5

// maxDNSMessage bounds a UDP DNS datagram read. 512 is the classic limit;
// the restricted resolver answers small A/AAAA sets, so we never need EDNS
// larger payloads. A bigger datagram from the guest is truncated/ignored.
const maxDNSMessage = 512

// DNSServer is the host-side restricted DNS server for allowlist-mode
// sandboxes. It answers the guest's queries on the per-sandbox gateway,
// resolving ONLY allowlisted names through the host Resolver (which
// rate-limits, flags NXDOMAIN bursts, and pins the result so the egress
// proxy admits the follow-up connection). Non-allowlisted names are refused
// without any upstream lookup. The hostile guest's packets are parsed with
// the Go team's dnsmessage codec, not a hand-rolled parser. Refs: SEC-04, SEC-07
type DNSServer struct {
	resolver *Resolver
	logger   *slog.Logger
}

// NewDNSServer validates dependencies and returns a DNSServer.
func NewDNSServer(resolver *Resolver, logger *slog.Logger) (*DNSServer, error) {
	switch {
	case resolver == nil:
		return nil, fmt.Errorf("egress dns server: resolver must not be nil")
	case logger == nil:
		return nil, fmt.Errorf("egress dns server: logger must not be nil")
	}
	return &DNSServer{resolver: resolver, logger: logger}, nil
}

// ServeUDP reads DNS datagrams until ctx is canceled or the socket closes,
// answering each. One malformed or hostile packet never stops the loop.
// Refs: SEC-07
func (s *DNSServer) ServeUDP(ctx context.Context, pc net.PacketConn) error {
	buf := make([]byte, maxDNSMessage)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_ = pc.SetReadDeadline(time.Now().Add(time.Second))
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue // deadline tick — re-check ctx
			}
			return fmt.Errorf("egress dns server: read: %w", err)
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		if resp := s.handleQuery(ctx, query); resp != nil {
			if _, err := pc.WriteTo(resp, addr); err != nil {
				s.logger.Warn("egress dns: write reply", "event", "dns_writefail", "error", err.Error())
			}
		}
	}
}

// handleQuery parses one query and builds the response bytes. It returns nil
// when the packet is too malformed to answer (no question), so the caller
// simply drops it. Only IN A / AAAA questions are answered; anything else is
// refused. Refs: SEC-04, SEC-07
func (s *DNSServer) handleQuery(ctx context.Context, query []byte) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil //nolint:nilerr // an unparseable query is dropped, not answered
	}
	q, err := p.Question()
	if err != nil {
		return nil //nolint:nilerr // a question-less/malformed query is dropped, not answered
	}
	rcode, answers := s.answer(ctx, q)
	return buildResponse(hdr.ID, q, rcode, answers)
}

// answer resolves an allowlisted name and returns the RCode plus the IPs to
// encode for the question's address family. A refused/failed resolution
// yields no answers and the matching RCode. Refs: SEC-04, SEC-07
func (s *DNSServer) answer(ctx context.Context, q dnsmessage.Question) (dnsmessage.RCode, []dnsmessage.Resource) {
	if q.Class != dnsmessage.ClassINET || (q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeAAAA) {
		return dnsmessage.RCodeRefused, nil
	}
	name := strings.TrimSuffix(q.Name.String(), ".")
	ips, err := s.resolver.Resolve(ctx, name)
	if err != nil {
		return rcodeForResolveErr(err), nil
	}

	var answers []dnsmessage.Resource
	for _, ip := range ips {
		ip = ip.Unmap()
		hdr := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: dnsAnswerTTL}
		switch {
		case q.Type == dnsmessage.TypeA && ip.Is4():
			hdr.Type = dnsmessage.TypeA
			answers = append(answers, dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AResource{A: ip.As4()}})
		case q.Type == dnsmessage.TypeAAAA && ip.Is6():
			hdr.Type = dnsmessage.TypeAAAA
			answers = append(answers, dnsmessage.Resource{Header: hdr, Body: &dnsmessage.AAAAResource{AAAA: ip.As16()}})
		}
	}
	// A resolved name with no record of the requested family is NOERROR with
	// an empty answer (the standard "no data" response), not an error.
	return dnsmessage.RCodeSuccess, answers
}

// rcodeForResolveErr maps a resolver refusal to a DNS RCode: a policy refusal
// (not allowlisted / rate-limited) is REFUSED; any other lookup failure
// (NXDOMAIN, server error) is NAMEERROR. Refs: SEC-07
func rcodeForResolveErr(err error) dnsmessage.RCode {
	if errors.Is(err, ErrNameNotAllowlisted) || errors.Is(err, ErrRateLimited) {
		return dnsmessage.RCodeRefused
	}
	return dnsmessage.RCodeNameError
}

// buildResponse encodes a DNS reply echoing the question, with the given
// RCode and answers. On an encode error it returns nil (the query is
// dropped) rather than a malformed datagram.
func buildResponse(id uint16, q dnsmessage.Question, rcode dnsmessage.RCode, answers []dnsmessage.Resource) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: id, Response: true, RecursionAvailable: true, RCode: rcode,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil
	}
	if err := b.Question(q); err != nil {
		return nil
	}
	if err := b.StartAnswers(); err != nil {
		return nil
	}
	for _, ans := range answers {
		switch body := ans.Body.(type) {
		case *dnsmessage.AResource:
			if b.AResource(ans.Header, *body) != nil {
				return nil
			}
		case *dnsmessage.AAAAResource:
			if b.AAAAResource(ans.Header, *body) != nil {
				return nil
			}
		}
	}
	msg, err := b.Finish()
	if err != nil {
		return nil
	}
	return msg
}
