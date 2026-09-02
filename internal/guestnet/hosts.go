package guestnet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHostsPath is the guest's static name table.
const DefaultHostsPath = "/etc/hosts"

// loopbackEntries is the minimum a POSIX userspace expects to exist without
// asking the network anything.
const loopbackEntries = "127.0.0.1\tlocalhost\n" +
	"::1\tlocalhost ip6-localhost ip6-loopback\n"

// EnsureHosts guarantees the guest can resolve localhost WITHOUT a DNS query.
//
// mgit composes its guest userspace from an OCI image, and container images
// routinely ship no /etc/hosts — the runtime normally supplies one. mgit is
// the runtime here and did not, so the file was absent entirely and every
// "localhost" fell through to DNS. Under the DEFAULT deny-all egress that
// lookup cannot succeed, so anything binding or dialing a local port failed
// with EAI_AGAIN: vitest, vite, jest, dev servers — in practice every JS
// project, and the error points at the network policy rather than at the
// missing file, which sends the reader somewhere else entirely.
//
// An image that ships its own hosts file has made a decision, so this ADDS
// what is missing and rewrites nothing: an existing localhost mapping is left
// exactly as found, and unrelated entries always survive. Refs: MGIT-159, SEC-04, FR-17.7
func EnsureHosts(path string) error {
	if path == "" {
		path = DefaultHostsPath
	}
	existing, err := os.ReadFile(path) //nolint:gosec // the guest's own name table at a fixed path
	switch {
	case err == nil:
		if mapsLocalhost(string(existing)) {
			return nil // the image decided; leave it alone
		}
		return writeHosts(path, prependLoopback(string(existing)))
	case os.IsNotExist(err):
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil { //nolint:gosec // /etc must be traversable by every user
			return fmt.Errorf("guest hosts: create %s: %w", filepath.Dir(path), mkErr)
		}
		return writeHosts(path, loopbackEntries)
	default:
		return fmt.Errorf("guest hosts: read %s: %w", path, err)
	}
}

// LocalhostEntry returns the first non-comment line of a hosts table whose
// HOSTNAME fields include localhost, or "" when no line maps it.
//
// It is what "the guest maps localhost" means when the table is read directly
// rather than through a resolver: a line like `127.0.0.1 localhost`. A comment
// that mentions the word, or a trailing `# localhost` remark, maps nothing —
// the exact false pass a resolver-free check exists to refuse. Exported for
// doctor's guest/localhost probe, so the writer and the checker agree on one
// definition. Refs: MGIT-169, MGIT-159
func LocalhostEntry(body string) string {
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "#") {
				break // the rest of the line is a remark, not a hostname
			}
			if field == "localhost" {
				return line
			}
		}
	}
	return ""
}

// mapsLocalhost reports whether any non-comment line already names localhost.
func mapsLocalhost(body string) bool { return LocalhostEntry(body) != "" }

// prependLoopback puts the loopback entries FIRST, which is where a resolver
// expects to find them, without disturbing the order of what follows.
func prependLoopback(existing string) string {
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	return loopbackEntries + existing
}

// writeHosts writes the table world-readable: every resolver in the guest
// reads it, and commands do not necessarily run as whoever wrote it.
func writeHosts(path, body string) error {
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // a name table only root can read is not a name table
		return fmt.Errorf("guest hosts: write %s: %w", path, err)
	}
	return nil
}
