// Package landwire is the single source of the sandbox land object-frame
// wire format (IDD-FR17-SANDBOX-PROTOCOL §1.1). Both ends import it — the
// guest land server (internal/guest) WRITES frames, and the host land
// decoder (internal/sandboxd/land) READS and bounds them — so the framing
// cannot drift between them (mirrors internal/execwire for the exec
// channel). A land payload is a stream of frames, each:
//
//	[1 byte: object type][4 bytes: big-endian uint32 length][payload]
//
// The guest is the hostile party and asserts no integrity: it serves only
// raw content-addressed git objects (SEC-01). All bounding and verification
// is host-side (land.Limits.DecodeObjects). Refs: FR-17.5, FR-17.35, SEC-01
package landwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Object type tags carried in the 1-byte frame header. Only these git
// object kinds are valid; any other tag is a schema violation.
const (
	ObjCommit byte = 'C'
	ObjTree   byte = 'T'
	ObjBlob   byte = 'B'
)

// HeaderLen is the fixed frame header: 1 type byte + 4-byte big-endian
// payload length.
const HeaderLen = 5

// ValidType reports whether t is an accepted object kind.
func ValidType(t byte) bool {
	switch t {
	case ObjCommit, ObjTree, ObjBlob:
		return true
	default:
		return false
	}
}

// WriteFrame writes one [type][len BE][payload] frame. The guest uses it to
// serve raw object bytes; it makes no integrity claim. A payload whose
// length exceeds a uint32 is refused (it cannot be framed). Refs: FR-17.5
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if !ValidType(typ) {
		return fmt.Errorf("landwire: invalid object type %#x", typ)
	}
	if int64(len(payload)) > int64(^uint32(0)) {
		return fmt.Errorf("landwire: object of %d bytes is unframable", len(payload))
	}
	var hdr [HeaderLen]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("landwire: write frame header: %w", err)
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("landwire: write frame payload: %w", err)
		}
	}
	return nil
}
