package egress

import (
	"context"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// fuzzQuery encodes a DNS question into wire bytes for the seed corpus. It
// panics on an encode error (seed construction, not the property under test).
func fuzzQuery(id uint16, name string, typ dnsmessage.Type, class dnsmessage.Class) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if b.StartQuestions() != nil {
		panic("seed: start questions")
	}
	n, err := dnsmessage.NewName(name)
	if err != nil {
		panic("seed: bad name " + name)
	}
	if b.Question(dnsmessage.Question{Name: n, Type: typ, Class: class}) != nil {
		panic("seed: question")
	}
	msg, err := b.Finish()
	if err != nil {
		panic("seed: finish")
	}
	return msg
}

// FuzzDNSServer_HandleQuery feeds arbitrary bytes — the hostile guest's DNS
// datagrams — to the restricted resolver's query handler. The handler parses
// untrusted input with the Go team's dnsmessage codec; the property is that no
// input panics or hangs, and any reply it emits is itself a well-formed DNS
// message echoing the query ID (a malformed reply would confuse the guest
// resolver and could itself be an injection vector). Pairs with the land-parser
// fuzz (MGIT-11.13.3). Refs: SEC-04, SEC-07, MGIT-11.13.6
func FuzzDNSServer_HandleQuery(f *testing.F) {
	// Seed corpus: allowlisted A/AAAA, wildcard match, non-allowlisted name,
	// a non-INET class, and degenerate/truncated inputs.
	f.Add(fuzzQuery(0x1234, "registry.npmjs.org.", dnsmessage.TypeA, dnsmessage.ClassINET))
	f.Add(fuzzQuery(0x1, "registry.npmjs.org.", dnsmessage.TypeAAAA, dnsmessage.ClassINET))
	f.Add(fuzzQuery(0x2, "pkg.go.golang.org.", dnsmessage.TypeA, dnsmessage.ClassINET))
	f.Add(fuzzQuery(0x3, "evil.example.com.", dnsmessage.TypeA, dnsmessage.ClassINET))
	f.Add(fuzzQuery(0x4, "registry.npmjs.org.", dnsmessage.TypeMX, dnsmessage.ClassINET))
	f.Add(fuzzQuery(0x5, "registry.npmjs.org.", dnsmessage.TypeA, dnsmessage.ClassCHAOS))
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x12, 0x34}) // truncated header
	f.Add(make([]byte, maxDNSMessage))

	al, err := Compile([]string{"registry.npmjs.org", "*.golang.org"})
	if err != nil {
		f.Fatal(err)
	}
	r, err := NewResolver(ResolverConfig{
		SandboxID: "01SB", TaskID: "MGIT-11.13.6",
		Allowlist: al, Lookup: resolvesTo("140.82.112.3"),
		Audit: &fakeAuditor{}, Clock: frozenClock(),
	})
	if err != nil {
		f.Fatal(err)
	}
	srv, err := NewDNSServer(r, quietLogger())
	if err != nil {
		f.Fatal(err)
	}
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, data []byte) {
		resp := srv.handleQuery(ctx, data)
		if resp == nil {
			return // an unparseable query is dropped — the documented contract
		}
		// A reply must be a well-formed DNS message that echoes the query ID.
		var p dnsmessage.Parser
		hdr, err := p.Start(resp)
		if err != nil {
			t.Fatalf("handleQuery emitted an unparseable response (%d bytes): %v", len(resp), err)
		}
		if !hdr.Response {
			t.Fatalf("handleQuery emitted a reply with the response bit unset")
		}
		// A reply is only built after a question parsed, so the query had a
		// valid header whose ID must be echoed.
		var qp dnsmessage.Parser
		qhdr, err := qp.Start(data)
		if err != nil {
			t.Fatalf("a reply was built for a query whose header does not parse")
		}
		if hdr.ID != qhdr.ID {
			t.Fatalf("reply ID %#x does not echo query ID %#x", hdr.ID, qhdr.ID)
		}
	})
}
