// Command netprobe is the GUEST-side network probe for the libkrun real-VM
// egress e2e. It is exec'd INSIDE a running sandbox through mgit-guest's vsock
// control plane, so — unlike testdata/netguest, which is its own PID 1 and
// configures eth0 itself — it does NO network setup whatsoever.
//
// That is the whole point: it can only reach anything if the PRODUCTION guest
// boot path (mgit-guest addressing eth0 from the host's boot tokens) actually
// worked. A probe that configured its own NIC would prove the test scaffolding
// works, which is exactly how MGIT-68 shipped.
//
// It lives under testdata/ so the normal build never compiles it; the test
// cross-compiles it for linux/<arch> on demand. Refs: MGIT-68, FR-17.8, SEC-04
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// dialTimeout bounds each probe. Long enough for a real transatlantic
// handshake, short enough that a black-holed flow fails inside the test's
// own deadline rather than hanging it.
const dialTimeout = 10 * time.Second

// headBytes is how much of the peer's reply is echoed back. Enough to show
// real bytes came from a real server, not enough to spam a console log.
const headBytes = 48

func main() {
	if len(os.Args) < 2 {
		fmt.Println("PROBE-RESULT USAGE = MISSING-VERB")
		os.Exit(2)
	}
	switch verb := os.Args[1]; verb {
	case "ifaces":
		reportInterfaces()
	case "dial":
		dial(arg(2))
	case "resolve":
		resolve(arg(2))
	case "fetch":
		fetch(arg(2))
	case "hold":
		hold(arg(2), arg(3))
	default:
		fmt.Printf("PROBE-RESULT USAGE = UNKNOWN-VERB %q\n", verb)
		os.Exit(2)
	}
}

// arg returns argv[i] or "" — a missing operand is reported by the probe
// itself rather than crashing, so the console still explains what happened.
func arg(i int) string {
	if len(os.Args) <= i {
		return ""
	}
	return os.Args[i]
}

// reportInterfaces dumps the guest's addressing. It is the diagnostic that
// distinguishes "policy refused the flow" from "the NIC was never configured"
// when a probe fails.
func reportInterfaces() {
	ifaces, err := net.Interfaces()
	if err != nil {
		fmt.Printf("PROBE-RESULT IFACES = ERROR %v\n", err)
		return
	}
	for _, i := range ifaces {
		addrs, _ := i.Addrs() //nolint:errcheck // an interface with no addrs reports none
		list := make([]string, 0, len(addrs))
		for _, a := range addrs {
			list = append(list, a.String())
		}
		fmt.Printf("PROBE-IFACE %s flags=%s addrs=%s\n", i.Name, i.Flags, strings.Join(list, ","))
	}
	if rc, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		fmt.Printf("PROBE-RESOLVCONF %q\n", strings.TrimSpace(string(rc)))
	} else {
		fmt.Printf("PROBE-RESOLVCONF = MISSING %v\n", err)
	}
	fmt.Println("PROBE-RESULT IFACES = OK")
}

// dial connects to a RAW host:port and exchanges real bytes. The reported
// reason on failure is what makes a policy refusal (connection refused: the
// gateway reset the handshake) distinguishable from a dead network (network
// is unreachable: no address or no route). Refs: MGIT-68
func dial(addr string) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		fmt.Printf("PROBE-RESULT DIAL = DENIED reason=%q\n", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	n, head, err := exchange(conn, hostOf(addr))
	if err != nil {
		fmt.Printf("PROBE-RESULT DIAL = CONNECTED-NO-DATA reason=%q\n", err.Error())
		return
	}
	fmt.Printf("PROBE-RESULT DIAL = ALLOWED bytes=%d head=%q\n", n, head)
}

// resolve asks the guest resolver — which must be the gateway's, via the
// /etc/resolv.conf mgit-guest writes — for a name's addresses.
func resolve(name string) {
	ips, err := net.LookupHost(name)
	if err != nil {
		fmt.Printf("PROBE-RESULT RESOLVE = FAILED reason=%q\n", err.Error())
		return
	}
	fmt.Printf("PROBE-RESULT RESOLVE = OK ips=%s\n", strings.Join(ips, ","))
}

// fetch is the full name path: resolve through the gateway resolver, connect
// to what it returned, and read real bytes back. It reports the address it
// actually connected to so the host can assert the resolver PINNED it
// (SEC-07) rather than the guest reaching somewhere else.
func fetch(addr string) {
	host := hostOf(addr)
	ips, err := net.LookupHost(host)
	if err != nil {
		fmt.Printf("PROBE-RESULT FETCH = RESOLVE-FAILED reason=%q\n", err.Error())
		return
	}
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		fmt.Printf("PROBE-RESULT FETCH = DENIED ips=%s reason=%q\n", strings.Join(ips, ","), err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	peer, _, _ := net.SplitHostPort(conn.RemoteAddr().String()) //nolint:errcheck // a dialed TCP addr always splits
	n, head, err := exchange(conn, host)
	if err != nil {
		fmt.Printf("PROBE-RESULT FETCH = CONNECTED-NO-DATA peer=%s reason=%q\n", peer, err.Error())
		return
	}
	fmt.Printf("PROBE-RESULT FETCH = ALLOWED peer=%s ips=%s bytes=%d head=%q\n",
		peer, strings.Join(ips, ","), n, head)
}

// hold is the ESTABLISHED-FLOW probe: it opens a connection, carries real
// bytes on it, announces that it is established, and then KEEPS IT OPEN while
// the host mutates policy underneath it — reporting whether the connection
// died or survived.
//
// WHY THIS VERB EXISTS. Every other verb here closes its connection before it
// returns, so a revoke issued between two probe calls has nothing established
// to terminate. That produced killed=0 on the first real-VM run of MGIT-72,
// which is indistinguishable between "there was nothing to kill" and "kill is
// broken" — and terminating the connection already carrying data is the entire
// meaning of "revoke means revoke".
//
// The announcement is a SEPARATE line written before the hold begins: the host
// streams the probe's stdout frame by frame, so it can wait for establishment
// and only then revoke. A fixed sleep on the host would race the handshake.
//
// KEEP-ALIVE, not a bare connect: the request is HTTP/1.1 with an explicit
// keep-alive so the destination itself holds the connection open for the
// duration. A server-side close during the window makes the DRAIN control fail
// loudly rather than making the KILL assertion pass by accident.
// Refs: MGIT-72, SEC-04
func hold(addr, seconds string) {
	window := 10 * time.Second
	if seconds != "" {
		if n, err := strconv.Atoi(seconds); err == nil && n > 0 {
			window = time.Duration(n) * time.Second
		}
	}
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		fmt.Printf("PROBE-RESULT HOLD = DIAL-DENIED reason=%q\n", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	n, head, err := exchangeKeepAlive(conn, hostOf(addr))
	if err != nil {
		fmt.Printf("PROBE-RESULT HOLD = CONNECTED-NO-DATA reason=%q\n", err.Error())
		return
	}
	// The host waits for THIS line before it mutates policy.
	fmt.Printf("PROBE-HOLD ESTABLISHED bytes=%d head=%q\n", n, head)

	watchConnection(conn, window)
}

// watchConnection polls an established connection for the window and reports
// whether it DIED or SURVIVED, with the elapsed time either way.
//
// A read timeout means the connection is still up (nothing to read is not
// death); any other read error is the connection going away, which is what a
// kill looks like from inside the guest — a reset or an EOF depending on how
// the host tore it down. Refs: MGIT-72
func watchConnection(conn net.Conn, window time.Duration) {
	start := time.Now()
	deadline := start.Add(window)
	buf := make([]byte, 1)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(pollInterval))
		_, err := conn.Read(buf)
		if err == nil {
			continue // real data: unambiguously alive
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			continue // nothing to read yet, still established
		}
		fmt.Printf("PROBE-RESULT HOLD = DIED after=%s reason=%q\n",
			time.Since(start).Round(time.Millisecond), err.Error())
		return
	}
	fmt.Printf("PROBE-RESULT HOLD = SURVIVED after=%s\n", time.Since(start).Round(time.Millisecond))
}

// pollInterval is how often the held connection is checked. Short enough that
// a kill is observed promptly, long enough not to spin.
const pollInterval = 250 * time.Millisecond

// exchangeKeepAlive is exchange with an explicit HTTP/1.1 keep-alive, so the
// destination does not close the connection after replying — the connection
// has to still be the host's to kill, not one the server already ended.
func exchangeKeepAlive(conn net.Conn, host string) (int, string, error) {
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if _, err := fmt.Fprintf(conn,
		"GET / HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", host); err != nil {
		return 0, "", err
	}
	buf := make([]byte, headBytes)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		return 0, "", err
	}
	return n, strings.TrimSpace(string(buf[:n])), nil
}

// exchange sends a minimal HTTP request and reads the first bytes of the
// reply. Reading REAL bytes is the assertion: a completed handshake alone
// would not prove the gateway spliced the flow to the actual destination.
func exchange(conn net.Conn, host string) (int, string, error) {
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: %s\r\n\r\n", host); err != nil {
		return 0, "", err
	}
	buf := make([]byte, headBytes)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		return 0, "", err
	}
	return n, strings.TrimSpace(string(buf[:n])), nil
}

// hostOf strips the port from a host:port, tolerating a bare host.
func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
