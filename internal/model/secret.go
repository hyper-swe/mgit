package model

import "log/slog"

// MaxStdinBytes bounds ExecRequest.Stdin: it carries a credential, which is small.
const MaxStdinBytes = 4096

// Secret is bytes that must never be rendered. Every formatting verb, the
// JSON encoding and a structured-log value show a fixed marker.
type Secret []byte

const redacted = "[redacted]"

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return redacted }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }
