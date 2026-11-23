package controlproto

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// KindReviewRun ('Z') asks the daemon to run one prepared review (MGIT-333): it
// re-verifies the sealed lease, consumes the nonce, starts the guest, runs the
// operator-mapped program and returns what it produced. What runs is never
// named by the caller; it comes from the mapping.
const KindReviewRun byte = 'Z'

// The limits of the run contract.
const (
	DefaultReviewSeconds = 600
	MaxReviewSeconds     = 1800
	// DefaultReviewOutputBytes and MaxReviewOutputBytes cap the reviewer's
	// standard output and standard error together.
	DefaultReviewOutputBytes int64 = 4 << 20
	MaxReviewOutputBytes     int64 = 16 << 20
)

// ReviewResultSchema names the result a run returns.
const ReviewResultSchema = "mgit.review-result/v1"

// ReviewRunArgs names a prepared lease and restates what the caller asked for
// when it prepared it, so the daemon can check the lease is exactly that.
type ReviewRunArgs struct {
	Profile        string `json:"profile"`
	Image          string `json:"image"`
	MappingID      string `json:"mapping_id"`
	BindingDigest  string `json:"binding_digest"`
	BundleDigest   string `json:"bundle_digest"`
	Nonce          string `json:"nonce"`
	Lease          string `json:"lease"`
	MaxSeconds     int    `json:"max_seconds,omitempty"`      // 0 = DefaultReviewSeconds
	MaxOutputBytes int64  `json:"max_output_bytes,omitempty"` // 0 = DefaultReviewOutputBytes
}

// ReviewRunResult carries either the result or the refusal envelope, each as the
// exact JSON the review package defines.
type ReviewRunResult struct {
	Result  json.RawMessage `json:"result,omitempty"`
	Refusal json.RawMessage `json:"refusal,omitempty"`
}

// ReviewResult is the body of a successful run: the reviewer ran to its end.
type ReviewResult struct {
	Schema    string `json:"schema"`
	Lease     string `json:"lease"`
	ExitCode  int    `json:"exit_code"`
	Signal    string `json:"signal"`
	Stdout    []byte `json:"stdout"` // base64 on the wire
	Stderr    []byte `json:"stderr"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

var reviewLeasePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// validateReviewRun bounds a run request at the protocol boundary.
func validateReviewRun(a *ReviewRunArgs) error {
	if a == nil {
		return fmt.Errorf("controlproto: review run request missing")
	}
	for name, v := range map[string]string{
		"profile": a.Profile, "image": a.Image, "binding_digest": a.BindingDigest,
		"bundle_digest": a.BundleDigest, "nonce": a.Nonce,
	} {
		if v == "" || len(v) > maxReviewField || strings.ContainsRune(v, 0) {
			return fmt.Errorf("controlproto: review run %s is missing or malformed", name)
		}
	}
	if !reviewIDPattern.MatchString(a.MappingID) {
		return fmt.Errorf("controlproto: review run mapping_id is missing or malformed")
	}
	if !reviewLeasePattern.MatchString(a.Lease) {
		return fmt.Errorf("controlproto: review run lease is missing or malformed")
	}
	if a.MaxSeconds < 0 || a.MaxSeconds > MaxReviewSeconds {
		return fmt.Errorf("controlproto: review run max_seconds is outside 0..%d", MaxReviewSeconds)
	}
	if a.MaxOutputBytes < 0 || a.MaxOutputBytes > MaxReviewOutputBytes {
		return fmt.Errorf("controlproto: review run max_output_bytes is outside 0..%d", MaxReviewOutputBytes)
	}
	return nil
}
