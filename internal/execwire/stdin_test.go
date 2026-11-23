package execwire

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/model"
)

func TestStdinRidesTheGuestRequestAndNotTheArgvOrEnv(t *testing.T) {
	const secret = "FAKE-KEY-0123456789-not-real"
	var buf bytes.Buffer
	in := model.ExecRequest{Command: []string{"mgit-guest", "cred-install"}, Stdin: model.Secret(secret)}
	if err := WriteRequest(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadRequest(&buf)
	if err != nil || string(out.Stdin) != secret {
		t.Fatalf("stdin did not arrive: %q %v", out.Stdin, err)
	}
	for _, a := range append(out.Command, out.Env...) {
		if strings.Contains(a, secret) {
			t.Fatal("secret in argv or env")
		}
	}
}

func TestARequestWithoutStdinHasTheShapeItAlwaysHad(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRequest(&buf, model.ExecRequest{Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "stdin") {
		t.Fatalf("an exec with no stdin grew a field: %q", buf.String())
	}
}

func TestAnOversizedStdinIsRefusedByTheReader(t *testing.T) {
	var buf bytes.Buffer
	raw := model.ExecRequest{Command: []string{"x"}, Stdin: make(model.Secret, model.MaxStdinBytes+1)}
	if err := WriteRequest(&buf, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(&buf); err == nil {
		t.Fatal("an oversized stdin was read")
	}
}

func TestWriteRequestLeavesTheCallersSecretIntact(t *testing.T) {
	// the wipe is of a copy: the caller's own bytes are not zeroed behind its back
	in := model.ExecRequest{Command: []string{"x"}, Stdin: model.Secret("FAKE-KEY-0123456789-not-real")}
	var buf bytes.Buffer
	if err := WriteRequest(&buf, in); err != nil {
		t.Fatal(err)
	}
	if string(in.Stdin) != "FAKE-KEY-0123456789-not-real" {
		t.Fatalf("the caller's secret was changed: %q", in.Stdin)
	}
}
