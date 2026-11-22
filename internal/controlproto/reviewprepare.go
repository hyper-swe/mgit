package controlproto

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// KindReviewPrepare ('W') asks the daemon to check a review request and sign
// what would run (MGIT-333). It starts nothing: a prepare that succeeds has
// only issued an attestation and sealed the evidence.
const KindReviewPrepare byte = 'W'

// ReviewPrepareArgs is one review request. The bundle is named by a host
// directory the daemon reads; it is never mounted into a guest.
type ReviewPrepareArgs struct {
	Profile       string `json:"profile"`
	Image         string `json:"image"`
	MappingID     string `json:"mapping_id"`
	BindingDigest string `json:"binding_digest"`
	BundleDir     string `json:"bundle_dir"`
	BundleDigest  string `json:"bundle_digest"`
	Nonce         string `json:"nonce"`
}

// ReviewPrepareResult carries either the signed attestation or the refusal
// envelope, each as the exact JSON the review package defines.
type ReviewPrepareResult struct {
	Attestation json.RawMessage `json:"attestation,omitempty"`
	Refusal     json.RawMessage `json:"refusal,omitempty"`
}

const (
	maxReviewField = 256
	maxReviewPath  = 4096
)

var reviewIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateReviewPrepare bounds a review request at the protocol boundary.
func validateReviewPrepare(a *ReviewPrepareArgs) error {
	if a == nil {
		return fmt.Errorf("controlproto: review prepare request missing")
	}
	for name, v := range map[string]string{
		"profile": a.Profile, "image": a.Image, "binding_digest": a.BindingDigest,
		"bundle_digest": a.BundleDigest, "nonce": a.Nonce,
	} {
		if v == "" || len(v) > maxReviewField || strings.ContainsRune(v, 0) {
			return fmt.Errorf("controlproto: review prepare %s is missing or malformed", name)
		}
	}
	if !reviewIDPattern.MatchString(a.MappingID) {
		return fmt.Errorf("controlproto: review prepare mapping_id is missing or malformed")
	}
	if a.BundleDir == "" || len(a.BundleDir) > maxReviewPath || strings.ContainsRune(a.BundleDir, 0) ||
		!filepath.IsAbs(a.BundleDir) {
		return fmt.Errorf("controlproto: review prepare bundle_dir must be an absolute path")
	}
	return nil
}
