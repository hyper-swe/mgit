package reviewbind

import (
	"bytes"
	"context"
	"errors"
	"io"
)

// StaticSource reads the one credential the operator provided, once, from r
// (an inherited descriptor), and returns a source that hands each run its own
// copy, only under the ref it was given for. A trailing newline is dropped. The
// daemon keeps the bytes in memory for its lifetime and never writes them out.
func StaticSource(ref string, r io.Reader) (CredentialSource, error) {
	if !refPattern.MatchString(ref) {
		return nil, errors.New("credential ref is malformed")
	}
	first := NewSlot(ref)
	if err := first.Fill(r); err != nil {
		return nil, err
	}
	master, err := first.Take()
	if err != nil {
		return nil, err
	}
	master = bytes.TrimRight(master, "\r\n")
	if len(master) == 0 {
		Wipe(master)
		return nil, errors.New("credential is empty or too large")
	}
	return func(_ context.Context, want string) (*Slot, error) {
		if want != ref {
			return nil, errors.New("no credential is provided for that ref")
		}
		s := NewSlot(ref)
		err := s.Fill(bytes.NewReader(master))
		return s, err
	}, nil
}
