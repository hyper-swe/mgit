package reviewbind

import (
	"errors"
	"io"
	"os"
	"sync"
)

// MaxSecret bounds a credential read from the fd.
const MaxSecret = 4096

// Slot holds one credential in memory between the run reading it and the
// guest taking it. It is filled once and taken once; it renders as its
// reference only, and no error carries its bytes.
type Slot struct {
	ref string
	mu  sync.Mutex
	b   []byte
	set bool
}

// NewSlot returns an empty slot named by the mapping's credential_ref.
func NewSlot(ref string) *Slot { return &Slot{ref: ref} }

// Fill reads the secret once from r.
func (s *Slot) Fill(r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set {
		return errors.New("credential slot is already filled")
	}
	b, err := io.ReadAll(io.LimitReader(r, MaxSecret+1))
	if err != nil {
		return errors.New("credential could not be read")
	}
	if len(b) == 0 || len(b) > MaxSecret {
		wipe(b)
		return errors.New("credential is empty or too large")
	}
	s.b, s.set = b, true
	return nil
}

// FillFromFile is Fill over an inherited descriptor; the caller owns its close.
func (s *Slot) FillFromFile(f *os.File) error { return s.Fill(f) }

// Take hands the secret over once and wipes the slot's copy afterwards.
func (s *Slot) Take() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set || s.b == nil {
		return nil, errors.New("credential slot is empty")
	}
	b := s.b
	s.b = nil
	return b, nil
}

// Wipe overwrites the bytes a caller got from Take once they are used.
func Wipe(b []byte) { wipe(b) }

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// String, GoString and MarshalJSON never include the secret.
func (s *Slot) String() string { return "credential-slot(" + s.ref + ")" }

// GoString implements fmt.GoStringer.
func (s *Slot) GoString() string { return s.String() }

// MarshalJSON implements json.Marshaler.
func (s *Slot) MarshalJSON() ([]byte, error) { return []byte(`"` + s.String() + `"`), nil }
