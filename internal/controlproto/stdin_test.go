package controlproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// A local client must not be able to send stdin to a sandbox exec. The control
// reader decodes with unknown fields refused, and the exec request has no
// stdin field in its encoding, so a request that names one is refused.
func TestAControlRequestCannotCarryStdin(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"exec":{"command":["x"],"stdin":"AAAA"}}`))
	dec.DisallowUnknownFields()
	var r Request
	if err := dec.Decode(&r); err == nil {
		t.Fatal("a control request carrying stdin was accepted")
	}
}
