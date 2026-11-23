package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const fakeSecret = "FAKE-KEY-0123456789-not-real"

func TestExecStdinIsNeverSerialisedByTheControlEncoding(t *testing.T) {
	req := ExecRequest{Command: []string{"x"}, Stdin: Secret(fakeSecret)}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), fakeSecret) || strings.Contains(strings.ToLower(string(b)), "stdin") {
		t.Fatalf("the control encoding carries stdin: %s", b)
	}
	var back ExecRequest
	if err := json.Unmarshal([]byte(`{"command":["x"],"stdin":"AAAA"}`), &back); err == nil && len(back.Stdin) != 0 {
		t.Fatal("a client could set stdin through JSON")
	}
}

func TestExecStdinIsRedactedInEveryRendering(t *testing.T) {
	req := ExecRequest{Command: []string{"x"}, Stdin: Secret(fakeSecret)}
	var sb strings.Builder
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		fmt.Fprintf(&sb, f, req)
		fmt.Fprintf(&sb, f, &req)
		fmt.Fprintf(&sb, f, req.Stdin)
	}
	if strings.Contains(sb.String(), fakeSecret) || strings.Contains(sb.String(), fmt.Sprintf("%x", fakeSecret)) {
		t.Fatalf("stdin leaked: %s", sb.String())
	}
}

func TestExecStdinIsBounded(t *testing.T) {
	ok := ExecRequest{Command: []string{"x"}, Stdin: make(Secret, MaxStdinBytes)}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	big := ExecRequest{Command: []string{"x"}, Stdin: make(Secret, MaxStdinBytes+1)}
	if err := big.Validate(); err == nil {
		t.Fatal("an oversized stdin was accepted")
	}
}

func TestExecStdinIsAbsentFromStructuredLogs(t *testing.T) {
	req := ExecRequest{Command: []string{"x"}, Stdin: Secret(fakeSecret)}
	for name, mk := range map[string]func(*bytes.Buffer) *slog.Logger{
		"json": func(b *bytes.Buffer) *slog.Logger { return slog.New(slog.NewJSONHandler(b, nil)) },
		"text": func(b *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(b, nil)) },
	} {
		var b bytes.Buffer
		mk(&b).Info("exec", "request", req, "stdin", req.Stdin, "ptr", &req)
		if strings.Contains(b.String(), fakeSecret) {
			t.Errorf("%s log leaked the stdin: %s", name, b.String())
		}
	}
}

func TestSecretIsRedactedUnderEveryFormatVerb(t *testing.T) {
	s := Secret(fakeSecret)
	type holder struct{ S Secret }
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%b", "%o", "%c", "%U", "%t", "%e", "%08.3d"} {
		for name, arg := range map[string]any{"value": s, "pointer": &s, "field": holder{s}, "fieldptr": &holder{s}, "slice": []Secret{s}} {
			out := fmt.Sprintf(verb, arg)
			for _, leak := range []string{fakeSecret, fmt.Sprintf("%d", []byte(fakeSecret)), fmt.Sprintf("%x", []byte(fakeSecret))} {
				if strings.Contains(out, leak) {
					t.Errorf("%s of a %s shows the secret: %s", verb, name, out)
				}
			}
		}
	}
}

func TestSecretMarkerIsExactlyTheDocumentedOne(t *testing.T) {
	s := Secret(fakeSecret)
	j, err := json.Marshal(s)
	if err != nil || string(j) != `"[redacted]"` {
		t.Errorf("json: %s %v", j, err)
	}
	tx, _ := s.MarshalText()
	for name, got := range map[string]string{
		"String": s.String(), "GoString": s.GoString(), "text": string(tx),
		"%v": fmt.Sprintf("%v", s), "%d": fmt.Sprintf("%d", s), "%x": fmt.Sprintf("%x", s),
		"log": s.LogValue().String(),
	} {
		if got != "[redacted]" {
			t.Errorf("%s renders %q, want the exact marker", name, got)
		}
	}
}
