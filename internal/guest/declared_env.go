package guest

import (
	"os"
	"path/filepath"
	"strings"
)

// DeclaredGuestEnvFile is where a composed base declares the environment its
// own toolchain needs, relative to the guest root.
//
// It lives INSIDE the composed tree on purpose. The base's content digest
// already covers every byte of that tree, so tampering with the declared PATH
// is exactly as detectable as tampering with a binary — no new signing input,
// no new trust decision, and no change to images.lock. Refs: MGIT-152, SEC-12
const DeclaredGuestEnvFile = "etc/mgit/guest-env"

// maxDeclaredPathLen bounds a declaration. A base is untrusted input; a
// multi-megabyte PATH is not a configuration, it is an attempt.
const maxDeclaredPathLen = 4096

// DeclaredGuestPath returns the PATH a base declared for itself, or "" when it
// declared none or declared one that cannot be honored safely.
//
// ONLY PATH is read. An OCI image's config carries a whole environment, and
// honoring it wholesale would let an untrusted base set LD_PRELOAD or
// LD_LIBRARY_PATH for every command mgit runs on the user's behalf. The
// problem being solved is "the toolchain is invisible", and PATH is the whole
// of it.
//
// A declaration is taken WHOLE or refused whole. Filtering out the bad
// elements of a PATH silently produces a third thing that neither the image
// nor mgit specified, and a base that declares a relative element is not
// making a subtle mistake — it is describing a search path that resolves
// against the process working directory, which a guest that chdirs can then
// shadow. Refs: MGIT-152
func DeclaredGuestPath(guestRoot string) string {
	data, err := os.ReadFile(filepath.Join(guestRoot, filepath.FromSlash(DeclaredGuestEnvFile))) //nolint:gosec // a fixed path inside the digest-verified base
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "PATH=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if validDeclaredPath(value) {
			return value
		}
		return ""
	}
	return ""
}

// validDeclaredPath reports whether every element is a non-empty absolute
// path and the whole is of a sane length. An EMPTY element is rejected for the
// same reason a relative one is: POSIX reads it as the current directory.
func validDeclaredPath(value string) bool {
	if value == "" || len(value) > maxDeclaredPathLen {
		return false
	}
	for _, part := range strings.Split(value, ":") {
		if part == "" || !strings.HasPrefix(part, "/") {
			return false
		}
	}
	return true
}

// baseEnvWith returns base with PATH replaced by declared, or base unchanged
// when declared is empty.
//
// It APPENDS rather than rewrites: the supervisor's own envValue and every
// exec consumer take the last occurrence of a key, so appending is the
// override. Refs: MGIT-152
func baseEnvWith(base []string, declared string) []string {
	if declared == "" {
		return base
	}
	out := make([]string, 0, len(base)+1)
	out = append(out, base...)
	return append(out, "PATH="+declared)
}
