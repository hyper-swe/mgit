// Package reviewbind holds what a review run needs from the operator beyond
// the sealed bundle: the installed binding map, the network policy and the
// credential slot. Nothing here reads from a bundle or a project.
package reviewbind

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hyper-swe/mgit/internal/reviewprep"
)

const maxMapBytes = 64 << 10

var (
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

var (
	hex64Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	imageDigestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// validateRecord holds a record to what prepare needs of it, so a map that
// installs or loads is one a review can use: lower-case hex digests and an
// absolute, clean entrypoint path.
func validateRecord(m *reviewprep.Mapping) error {
	switch {
	case !hex64Pattern.MatchString(m.BindingSHA256):
		return bad("mapping needs binding_sha256 as 64 lower-case hex digits")
	case !imageDigestShape.MatchString(m.ImageDigest):
		return bad("mapping needs image_digest as sha256:<64 lower-case hex digits>")
	case !hex64Pattern.MatchString(m.EntrypointSHA256):
		return bad("mapping needs entrypoint_sha256 as 64 lower-case hex digits")
	case !path.IsAbs(m.GuestEntrypoint) || path.Clean(m.GuestEntrypoint) != m.GuestEntrypoint:
		return bad("mapping needs guest_entrypoint as an absolute, clean path")
	}
	return nil
}

func bad(detail string) error { return reviewprep.Refuse(reviewprep.CodeMappingMismatch, detail) }

// CanonicalHashJSON is the sha256 (hex) of the record's canonical form: keys
// sorted, no insignificant space, so how the operator laid the file out does
// not change it.
func CanonicalHashJSON(raw []byte) (string, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", errors.New("bytes after the record")
	}
	canon, err := json.Marshal(v) // map keys are emitted sorted
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalHash is CanonicalHashJSON of the record m.
func CanonicalHash(m reviewprep.Mapping) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return CanonicalHashJSON(raw)
}

// readPlain reads a regular, non-symlink file that only its owner can write.
func readPlain(p string, max int64) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 || fi.Size() > max {
		return nil, bad("mapping is not installed as a plain owner-writable file")
	}
	return os.ReadFile(p) //nolint:gosec // p is built from a checked id under the operator's directory and was just inspected
}

// LoadMap reads the operator-installed map id under hostRoot/review/maps. The
// record is accepted only if it is pinned: a sidecar <id>.sha256 holds the
// canonical hash the operator recorded at install, and the record must still
// match it. A map that is missing, edited since, linked, writable by others,
// named another id or asks for a malformed egress policy or credential is
// refused with MAPPING_MISMATCH.
func LoadMap(hostRoot, id string) (*reviewprep.Mapping, error) {
	if !idPattern.MatchString(id) {
		return nil, bad("mapping is not installed")
	}
	for _, d := range []string{hostRoot, filepath.Join(hostRoot, "review"), filepath.Join(hostRoot, "review", "maps")} {
		fi, derr := os.Lstat(d)
		if derr != nil || !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
			return nil, bad("mapping directory " + d + " must be a plain directory only its owner can write; run: chmod 0700 " + d)
		}
	}
	dir := filepath.Join(hostRoot, "review", "maps")
	raw, err := readPlain(filepath.Join(dir, id+".json"), maxMapBytes)
	if err != nil {
		return nil, err
	}
	pin, err := readPlain(filepath.Join(dir, id+".sha256"), 256)
	if err != nil {
		return nil, bad("mapping has no pinned hash")
	}
	got, err := CanonicalHashJSON(raw)
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(pin)), got) {
		return nil, bad("mapping does not match its pinned hash")
	}
	var m reviewprep.Mapping
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil || m.Schema != reviewprep.MapSchema || m.ID != id {
		return nil, bad("mapping is not a valid map record for this id")
	}
	// The pinned hash covers every key the file holds; the struct decode takes
	// keys by any letter case and the last duplicate. Accept only a record that
	// is exactly its own decoded form, so what was pinned is what is used.
	if again, herr := CanonicalHash(m); herr != nil || !strings.EqualFold(again, got) {
		return nil, bad("mapping has keys the loader does not read as written")
	}
	if err := validateRecord(&m); err != nil {
		return nil, err
	}
	if _, err := ParseEgress(m.EgressPolicyID); err != nil {
		return nil, bad("mapping names a malformed egress policy")
	}
	if m.CredentialRef != "" {
		if !refPattern.MatchString(m.CredentialRef) {
			return nil, bad("mapping names a malformed credential reference")
		}
		if p, _ := ParseEgress(m.EgressPolicyID); p.Kind != EgressHost {
			return nil, bad("a credential needs a named egress host")
		}
	}
	return &m, nil
}
