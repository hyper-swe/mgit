package land

import (
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/hyper-swe/mgit/internal/landwire"
	"github.com/hyper-swe/mgit/internal/model"
)

// Object type tags carried in the land frame header, single-sourced from
// the shared wire package so the guest writer and this host reader cannot
// drift. Only known git object kinds are accepted; an unknown tag is a
// schema violation.
const (
	ObjCommit = landwire.ObjCommit
	ObjTree   = landwire.ObjTree
	ObjBlob   = landwire.ObjBlob
)

// Object is one raw git object read off the land channel, within the
// configured ceilings. Hashing/verification is land.VerifyCommit's job
// (MGIT-11.8.3); this layer only frames and bounds.
type Object struct {
	Type byte
	Data []byte
}

// Limits are the land-protocol ceilings (FR-17.35). They are a struct,
// not constants, because they are host-configurable per FR-17.13.
type Limits struct {
	MaxObjectBytes    int   // per-object cap (default 64 MiB)
	MaxObjectsPerLand int   // objects per land (default 100k)
	MaxTotalBytes     int64 // aggregate payload cap (default 4 GiB)
}

// DefaultLimits returns the FR-17.35 default ceilings.
func DefaultLimits() Limits {
	return Limits{
		MaxObjectBytes:    64 << 20,
		MaxObjectsPerLand: 100_000,
		MaxTotalBytes:     4 << 30,
	}
}

// frameHeaderLen is the fixed land frame header: 1 type byte + 4-byte
// big-endian payload length (single-sourced from landwire).
const frameHeaderLen = landwire.HeaderLen

// DecodeObjects reads length-framed objects from r — each frame is
// [1 type byte][4-byte BE length][payload] — enforcing all three
// FR-17.35 ceilings as it goes: per-object size, object count, and
// aggregate bytes (zip-bomb class). Unknown object types and truncated
// framing are schema violations. Every rejection is
// ErrLandVerificationFailed; nothing partial is returned on error.
// The guest is untrusted, so a declared length over the per-object cap
// is refused BEFORE any bytes are read for it. Refs: F-03, FR-17.35
func (l Limits) DecodeObjects(r io.Reader) ([]Object, error) {
	var (
		objs  []Object
		total int64
	)
	for {
		var hdr [frameHeaderLen]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return objs, nil // clean end of stream
			}
			return nil, fmt.Errorf("%w: truncated frame header: %w", model.ErrLandVerificationFailed, err)
		}
		typ := hdr[0]
		if !landwire.ValidType(typ) {
			return nil, fmt.Errorf("%w: unknown object type %#x", model.ErrLandVerificationFailed, typ)
		}
		length := binary.BigEndian.Uint32(hdr[1:])
		if int64(length) > int64(l.MaxObjectBytes) {
			return nil, fmt.Errorf("%w: object of %d bytes exceeds the %d cap",
				model.ErrLandVerificationFailed, length, l.MaxObjectBytes)
		}
		if len(objs)+1 > l.MaxObjectsPerLand {
			return nil, fmt.Errorf("%w: more than %d objects per land",
				model.ErrLandVerificationFailed, l.MaxObjectsPerLand)
		}
		total += int64(length)
		if total > l.MaxTotalBytes {
			return nil, fmt.Errorf("%w: total payload exceeds the %d cap",
				model.ErrLandVerificationFailed, l.MaxTotalBytes)
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("%w: truncated object body: %w", model.ErrLandVerificationFailed, err)
		}
		objs = append(objs, Object{Type: typ, Data: data})
	}
}

// ValidateTreePath enforces that a git tree-entry path is a canonical,
// relative, worktree-confined path: slash-separated, no traversal
// (".."), no absolute prefix, and no non-canonical encodings (".", "./",
// "//", trailing slash, backslash, NUL). A guest could otherwise smuggle
// a write outside the worktree or past the host-trusted-path checks
// (NFR-5.6 applied at the land boundary, T8). Refs: FR-17.35, NFR-5.6
func ValidateTreePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty tree path", model.ErrLandVerificationFailed)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%w: tree path contains NUL", model.ErrLandVerificationFailed)
	case strings.ContainsRune(p, '\\'):
		return fmt.Errorf("%w: tree path contains a backslash (non-canonical)", model.ErrLandVerificationFailed)
	case path.IsAbs(p):
		return fmt.Errorf("%w: tree path %q is absolute", model.ErrLandVerificationFailed, p)
	case path.Clean(p) != p:
		// Clean collapses "./", "//", trailing "/", and resolves ".." —
		// any difference means the input was non-canonical.
		return fmt.Errorf("%w: tree path %q is not canonical", model.ErrLandVerificationFailed, p)
	}
	for _, comp := range strings.Split(p, "/") {
		if comp == ".." {
			return fmt.Errorf("%w: tree path %q escapes the worktree", model.ErrLandVerificationFailed, p)
		}
	}
	return nil
}
