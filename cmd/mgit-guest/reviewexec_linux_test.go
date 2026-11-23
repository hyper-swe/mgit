//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hyper-swe/mgit/internal/reviewlaunch"
)

// TestMain lets this test binary also be the launcher's re-exec'd stage, as
// main does first thing. Without it the launcher starts this binary as its
// stage, the binary runs the whole suite again instead, and every run starts
// another: the tests multiply until the machine is out of processes.
func TestMain(m *testing.M) {
	if reviewlaunch.MaybeStage() {
		return
	}
	os.Exit(m.Run())
}

// refusal runs review-exec against a digest that cannot match, which the
// launcher refuses before it starts anything.
func refusal(t *testing.T, nonce string) (int, map[string]any, string) {
	t.Helper()
	if nonce != "" {
		t.Setenv("MGIT_REVIEW_REFUSAL_NONCE", nonce)
	} else {
		t.Setenv("MGIT_REVIEW_REFUSAL_NONCE", "")
		os.Unsetenv("MGIT_REVIEW_REFUSAL_NONCE")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := reviewExec([]string{"--sha256", strings.Repeat("0", 64), "--", self}, strings.NewReader(""), &out, &errb)
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(errb.Bytes()), &env); err != nil {
		t.Fatalf("stderr %q: %v", errb.String(), err)
	}
	return code, env, errb.String()
}

func TestReviewExec_RefusalCarriesTheNonceItWasGiven(t *testing.T) {
	code, env, _ := refusal(t, "abc123abc123abc123abc123abc123ab")
	if code != 70 || env["nonce"] != "abc123abc123abc123abc123abc123ab" || env["code"] != "HASH_MISMATCH" {
		t.Fatalf("code=%d env=%v", code, env)
	}
}

func TestReviewExec_RefusalWithoutANonceCarriesNone(t *testing.T) {
	_, env, _ := refusal(t, "")
	if _, ok := env["nonce"]; ok {
		t.Fatalf("env %v", env)
	}
}

func TestReviewExec_TheNonceDoesNotSurviveIntoTheProcessEnvironment(t *testing.T) {
	refusal(t, "abc123abc123abc123abc123abc123ab")
	if v, ok := os.LookupEnv("MGIT_REVIEW_REFUSAL_NONCE"); ok {
		t.Fatalf("still set: %q", v)
	}
}
