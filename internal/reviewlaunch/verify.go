package reviewlaunch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
)

var canonicalDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// VerifyEntrypoint hashes the bytes of f, the very descriptor that will be
// executed, and refuses unless they equal want (lower-case hex SHA-256) and
// begin with the ELF magic. A script would need an interpreter, which is a
// second launch the profile does not allow. The read position is rewound.
func VerifyEntrypoint(f *os.File, want string) error {
	if !canonicalDigest.MatchString(want) {
		return refuse(CodeBadDigest, "entrypoint digest is not 64 lower-case hex digits")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return refuse(CodeLaunchFailed, "entrypoint is not readable: %v", err)
	}
	h := sha256.New()
	magic := make([]byte, 4)
	n, err := io.ReadFull(io.TeeReader(f, h), magic)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return refuse(CodeLaunchFailed, "entrypoint is not readable: %v", err)
	}
	if _, err := io.Copy(h, f); err != nil {
		return refuse(CodeLaunchFailed, "entrypoint is not readable: %v", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return refuse(CodeHashMismatch, "entrypoint digest differs from the pinned one")
	}
	if n < 4 || string(magic) != "\x7fELF" {
		return refuse(CodeNotELF, "entrypoint is not an ELF executable")
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

// RefusalNonceEnv names the environment variable by which the caller hands the
// launcher a per-run secret. The launcher echoes it in its refusal envelope so
// the caller can tell that envelope from bytes the reviewed program prints on
// the same stream; the program never sees it (its environment is empty).
const RefusalNonceEnv = "MGIT_REVIEW_REFUSAL_NONCE"

// RefusalJSON renders err as the mgit.review-error/v1 envelope for a launch
// that did not start the reviewer. It carries no prompt, bundle or credential.
func RefusalJSON(r *RefusalError) []byte { return RefusalJSONWithNonce(r, "") }

// RefusalJSONWithNonce is RefusalJSON with the caller's per-run nonce echoed.
func RefusalJSONWithNonce(r *RefusalError, nonce string) []byte {
	b, _ := json.Marshal(struct {
		Schema          string `json:"schema"`
		Code            string `json:"code"`
		Phase           string `json:"phase"`
		ReviewerStarted bool   `json:"reviewer_started"`
		Detail          string `json:"detail"`
		Nonce           string `json:"nonce,omitempty"`
	}{"mgit.review-error/v1", r.Code, "launch", false, r.Detail, nonce})
	return b
}
