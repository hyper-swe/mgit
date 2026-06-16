package land

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/model"
)

// frame encodes one object frame: [1 type byte][4-byte BE length][bytes].
func frame(typ byte, data []byte) []byte {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(data)))
	return append(hdr[:], data...)
}

func testLimits() Limits {
	// Small ceilings so the tests exercise them without huge payloads.
	return Limits{MaxObjectBytes: 64, MaxObjectsPerLand: 3, MaxTotalBytes: 256}
}

// TestLandProto_OversizePayload_Rejected verifies a single object whose
// declared length exceeds the per-object ceiling is refused. Refs: F-03, FR-17.35
func TestLandProto_OversizePayload_Rejected(t *testing.T) {
	lim := testLimits()
	var buf bytes.Buffer
	buf.Write(frame(ObjCommit, bytes.Repeat([]byte("x"), lim.MaxObjectBytes+1)))

	_, err := lim.DecodeObjects(&buf)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestLandProto_ObjectCountCeiling_Rejected verifies more objects than
// the per-land ceiling is refused (zip-bomb class). Refs: F-03, FR-17.35
func TestLandProto_ObjectCountCeiling_Rejected(t *testing.T) {
	lim := testLimits() // ceiling is three objects
	var buf bytes.Buffer
	for i := 0; i < lim.MaxObjectsPerLand+1; i++ {
		buf.Write(frame(ObjBlob, []byte("a")))
	}
	_, err := lim.DecodeObjects(&buf)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestLandProto_TotalBytesCeiling_Rejected verifies the aggregate payload
// ceiling is enforced even when each object is individually small.
func TestLandProto_TotalBytesCeiling_Rejected(t *testing.T) {
	lim := Limits{MaxObjectBytes: 64, MaxObjectsPerLand: 1000, MaxTotalBytes: 100}
	var buf bytes.Buffer
	for i := 0; i < 5; i++ {
		buf.Write(frame(ObjBlob, bytes.Repeat([]byte("y"), 40))) // five 40-byte objects exceed the 100-byte total
	}
	_, err := lim.DecodeObjects(&buf)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
}

// TestLandProto_WellFormed_Decodes verifies a conforming stream decodes
// to the expected objects.
func TestLandProto_WellFormed_Decodes(t *testing.T) {
	lim := testLimits()
	var buf bytes.Buffer
	buf.Write(frame(ObjCommit, []byte("commit-bytes")))
	buf.Write(frame(ObjTree, []byte("tree")))

	objs, err := lim.DecodeObjects(&buf)
	require.NoError(t, err)
	require.Len(t, objs, 2)
	assert.Equal(t, ObjCommit, objs[0].Type)
	assert.Equal(t, "commit-bytes", string(objs[0].Data))
	assert.Equal(t, ObjTree, objs[1].Type)
}

// TestLandProto_Malformed_Rejected covers truncated framing and an
// unknown object type. Refs: F-03
func TestLandProto_Malformed_Rejected(t *testing.T) {
	lim := testLimits()
	t.Run("truncated_header", func(t *testing.T) {
		_, err := lim.DecodeObjects(bytes.NewReader([]byte{ObjCommit, 0x00})) // partial length
		assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
	})
	t.Run("truncated_body", func(t *testing.T) {
		var hdr [5]byte
		hdr[0] = ObjBlob
		binary.BigEndian.PutUint32(hdr[1:], 10) // claims 10 bytes
		_, err := lim.DecodeObjects(bytes.NewReader(append(hdr[:], []byte("short")...)))
		assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
	})
	t.Run("unknown_object_type", func(t *testing.T) {
		_, err := lim.DecodeObjects(bytes.NewReader(frame('Z', []byte("x"))))
		assert.ErrorIs(t, err, model.ErrLandVerificationFailed)
	})
}

// TestLandProto_TraversalPath_Rejected verifies tree-entry paths that
// escape the worktree are refused. Refs: NFR-5.6, FR-17.35
func TestLandProto_TraversalPath_Rejected(t *testing.T) {
	for _, p := range []string{
		"../etc/passwd",
		"a/../../b",
		"foo/..",
		"..",
	} {
		t.Run(p, func(t *testing.T) {
			assert.ErrorIs(t, ValidateTreePath(p), model.ErrLandVerificationFailed)
		})
	}
}

// TestLandProto_NonCanonicalEncoding_Rejected verifies non-canonical or
// unsafe path encodings are refused. Refs: NFR-5.6, FR-17.35
func TestLandProto_NonCanonicalEncoding_Rejected(t *testing.T) {
	for _, p := range []string{
		"",          // empty
		"/abs/path", // absolute
		"./a",       // non-canonical leading ./
		"a//b",      // empty component
		"a/",        // trailing slash
		"a/./b",     // embedded .
		"a\\b",      // backslash
		"a\x00b",    // NUL
	} {
		t.Run(strings.ReplaceAll(p, "\x00", "<NUL>"), func(t *testing.T) {
			assert.ErrorIs(t, ValidateTreePath(p), model.ErrLandVerificationFailed)
		})
	}
}

// TestLandProto_CanonicalPath_Accepted verifies ordinary relative paths
// pass.
func TestLandProto_CanonicalPath_Accepted(t *testing.T) {
	for _, p := range []string{"main.go", "src/app/main.go", "a/b/c.txt", ".github/workflows/ci.yml"} {
		t.Run(p, func(t *testing.T) { assert.NoError(t, ValidateTreePath(p)) })
	}
}

// TestDefaultLimits_AreFR1735Defaults verifies the shipped ceilings match
// the FR-17.35 defaults (64 MiB / 100k objects / 4 GiB).
func TestDefaultLimits_AreFR1735Defaults(t *testing.T) {
	l := DefaultLimits()
	assert.Equal(t, 64<<20, l.MaxObjectBytes)
	assert.Equal(t, 100_000, l.MaxObjectsPerLand)
	assert.Equal(t, int64(4)<<30, l.MaxTotalBytes)
}
