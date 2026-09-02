package controlproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// KindEcho ('M') asks the daemon for a control response of an EXACT encoded
// size, and it exists for one reader: `mgit doctor`.
//
// MGIT-160 was a response the daemon could not send — a 1 MiB cap, refused
// daemon-side and seen client-side as a bare EOF — and R-H300 rule 5 wants
// that incident to become something the tool asks itself. "Can this daemon
// answer its largest legal response" is a PROPERTY, not a state; the way to
// ask it as a state is to answer one, now, at the cap, through the real
// channel, and verify it arrived — then ask for one byte more and verify the
// refusal is legible. This verb is that question.
//
// 'M' is a letter no frame family uses (execwire: E/O/R/H; landwire: C/T/B;
// every other kind above), so a misrouted frame stays recognizable.
// Refs: MGIT-175, MGIT-160, R-H300
const KindEcho byte = 'M'

// MaxEchoBytes bounds what an echo may ask for: enough to provoke the
// over-cap refusal, never enough to drive a large allocation on the daemon
// that supervises every VM. Refs: MGIT-175, MGIT-11.10.7
const MaxEchoBytes = 2 * MaxResponseBytes

// echoFillByte is the fill's only byte. It needs no JSON escaping, so the
// encoded size is the fill's length plus a constant envelope.
const echoFillByte = "a"

// ErrEchoDigest means an echo answer's fill does not hash to its digest: it
// was altered or truncated somewhere between the daemon and the reader.
var ErrEchoDigest = errors.New("echo digest does not match its fill")

// EchoArgs asks for a response whose ENCODED size is exactly Bytes.
type EchoArgs struct {
	Bytes int `json:"bytes"`
}

// Validate refuses a negative or unbounded request before anything is built.
func (a *EchoArgs) Validate() error {
	if a == nil {
		return errors.New("controlproto: echo request missing payload")
	}
	if a.Bytes < 0 {
		return fmt.Errorf("controlproto: echo of %d bytes is negative", a.Bytes)
	}
	if a.Bytes > MaxEchoBytes {
		return fmt.Errorf("controlproto: echo of %d bytes exceeds the %d-byte echo bound", a.Bytes, MaxEchoBytes)
	}
	return nil
}

// EchoResult is the answer: a fill sized so the whole encoded response is
// exactly Bytes long, and the fill's SHA-256, so a reader can verify the
// answer arrived byte-intact and at the size that was asked for.
type EchoResult struct {
	Bytes  int    `json:"bytes"`
	Digest string `json:"digest"`
	Fill   string `json:"fill"`
}

// BuildEchoResponse builds a Response whose JSON encoding is exactly n bytes.
//
// The envelope — every byte that is not fill — is measured by encoding once
// with an empty fill and a same-length digest, then the fill is sized to the
// remainder. A request smaller than the envelope cannot be met exactly and is
// refused rather than rounded up. Sizes over MaxResponseBytes are built on
// purpose: WriteResponse is the layer that refuses them, and provoking that
// refusal is half of what the doctor check asks. Refs: MGIT-175, MGIT-160
func BuildEchoResponse(n int) (*Response, error) {
	envelope, err := json.Marshal(&Response{Echo: &EchoResult{Bytes: n, Digest: echoDigest("")}})
	if err != nil {
		return nil, fmt.Errorf("controlproto: encode echo envelope: %w", err)
	}
	fillLen := n - len(envelope)
	if fillLen < 0 {
		return nil, fmt.Errorf("controlproto: an echo of %d bytes is smaller than the %d-byte response envelope", n, len(envelope))
	}
	fill := strings.Repeat(echoFillByte, fillLen)
	return &Response{Echo: &EchoResult{Bytes: n, Digest: echoDigest(fill), Fill: fill}}, nil
}

// VerifyEcho reports whether an answer is byte-intact: the fill hashes to
// the digest, and the answer re-encodes to exactly the size it claims. Both
// sides use the same encoder, so a truncated or altered answer cannot verify.
func VerifyEcho(res *EchoResult) error {
	if res == nil {
		return errors.New("no echo answer")
	}
	if echoDigest(res.Fill) != res.Digest {
		return ErrEchoDigest
	}
	enc, err := json.Marshal(&Response{Echo: res})
	if err != nil {
		return fmt.Errorf("controlproto: encode echo answer: %w", err)
	}
	if len(enc) != res.Bytes {
		return fmt.Errorf("echo answer is %d bytes, not the %d it claims", len(enc), res.Bytes)
	}
	return nil
}

func echoDigest(fill string) string {
	sum := sha256.Sum256([]byte(fill))
	return hex.EncodeToString(sum[:])
}
