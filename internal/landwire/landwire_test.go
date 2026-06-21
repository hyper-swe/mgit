package landwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidType(t *testing.T) {
	for _, ty := range []byte{ObjCommit, ObjTree, ObjBlob} {
		assert.True(t, ValidType(ty))
	}
	assert.False(t, ValidType('Z'))
	assert.False(t, ValidType(0))
}

// TestWriteFrame_Roundtrips verifies a written frame decodes to the same
// type and payload bytes (header is [type][4-byte BE len]).
func TestWriteFrame_Roundtrips(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteFrame(&buf, ObjCommit, []byte("hello")))
	out := buf.Bytes()
	require.Len(t, out, HeaderLen+5)
	assert.Equal(t, ObjCommit, out[0])
	assert.Equal(t, byte(5), out[4], "big-endian length low byte")
	assert.Equal(t, "hello", string(out[HeaderLen:]))
}

func TestWriteFrame_EmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteFrame(&buf, ObjTree, nil))
	assert.Len(t, buf.Bytes(), HeaderLen)
}

func TestWriteFrame_InvalidType_Error(t *testing.T) {
	var buf bytes.Buffer
	assert.Error(t, WriteFrame(&buf, 'X', []byte("x")))
}

// failWriter fails after allowing n bytes, to exercise write-error paths.
type failWriter struct{ left int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, errAssert
	}
	n := len(p)
	if n > f.left {
		n = f.left
	}
	f.left -= n
	if n < len(p) {
		return n, errAssert
	}
	return n, nil
}

var errAssert = bytes.ErrTooLarge

func TestWriteFrame_HeaderWriteError(t *testing.T) {
	assert.Error(t, WriteFrame(&failWriter{left: 0}, ObjBlob, []byte("data")))
}

func TestWriteFrame_PayloadWriteError(t *testing.T) {
	assert.Error(t, WriteFrame(&failWriter{left: HeaderLen}, ObjBlob, []byte("data")))
}
