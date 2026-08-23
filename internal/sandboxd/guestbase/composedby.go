package guestbase

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// composedByFile records the substrate that built a base, relative to the
// base root.
//
// It lives INSIDE the composed tree, following the precedent MGIT-152 set for
// the declared PATH: the base's content digest already covers every byte of
// that tree, so tampering with the marker is exactly as detectable as
// tampering with a binary — no images.lock change, no signing-payload change,
// and no existing registration invalidated. Refs: MGIT-174, MGIT-152, SEC-12
const composedByFile = "etc/mgit/composed-by.json"

// ErrComposedByUnknown means a base does not say what composed it — because it
// predates this marker, or because the marker cannot be read.
//
// It is a distinct outcome from "current" on purpose. Reporting silence as
// currency is the precise failure being fixed: the staleness door never fired
// for two releases, and the absence of a warning was read as an assurance.
// Refs: MGIT-174
var ErrComposedByUnknown = errors.New("this base does not record what composed it")

// ComposedBy is a base's record of its own origin.
type ComposedBy struct {
	// Version is the mgit substrate that composed the base. It matters because
	// the guest binaries are injected AT COMPOSE TIME and frozen there, so this
	// is also the version of the guest code that will run inside the VM.
	Version string `json:"version"`
	// Source is the OCI reference it was composed from, when there was one.
	Source string `json:"source,omitempty"`
	// There is deliberately NO TIMESTAMP.
	//
	// The marker lives inside the composed tree, so every byte of it is part
	// of the base's content digest — and a timestamp would make two composes
	// of the same image produce different digests. That breaks the property
	// the whole base cache rests on: identity is content, so recomposing the
	// same inputs must reproduce the same pin (MGIT-147). Three existing
	// determinism tests caught this within minutes of it being written.
	//
	// Nothing is lost. The question this marker answers is WHICH SUBSTRATE
	// composed the base, because that is what fixes the guest binaries frozen
	// into it; "when" is a property of the registration, not of the bytes.
	// Refs: MGIT-174, MGIT-147
}

// WriteComposedBy records the composing substrate into a staged base tree.
func WriteComposedBy(baseDir, version, source string) error {
	rec := ComposedBy{Version: version, Source: source}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("guest base: encode composed-by: %w", err)
	}
	full := filepath.Join(baseDir, filepath.FromSlash(composedByFile))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return fmt.Errorf("guest base: create %s: %w", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("guest base: write composed-by: %w", err)
	}
	return nil
}

// ReadComposedBy reads a base's origin record, or ErrComposedByUnknown.
func ReadComposedBy(baseDir string) (ComposedBy, error) {
	var rec ComposedBy
	data, err := os.ReadFile(filepath.Join(baseDir, filepath.FromSlash(composedByFile))) //nolint:gosec // fixed path inside a digest-verified base
	if err != nil {
		return rec, ErrComposedByUnknown
	}
	if err := json.Unmarshal(data, &rec); err != nil || rec.Version == "" {
		// A marker we cannot parse tells us nothing. Saying so beats guessing.
		return ComposedBy{}, ErrComposedByUnknown
	}
	return rec, nil
}

// Currency is how a base's composing substrate relates to the running one.
type Currency string

const (
	// CurrencyCurrent means the base was composed by the substrate now running it.
	CurrencyCurrent Currency = "current"
	// CurrencyStale means they differ, so the guest binaries frozen into this base
	// are not this substrate's — and the base silently lacks the guest-side
	// changes of every release between them.
	CurrencyStale Currency = "stale"
	// CurrencyUnknown means one side is not recorded. Never treated as current.
	CurrencyUnknown Currency = "unknown"
)

// BaseCurrency compares a base's composing substrate to the running one.
//
// ANY difference is stale, in either direction. The question is not "is the
// base old" but "do the guest binaries inside it match the substrate driving
// them" — and they do not whenever the versions disagree, whichever is newer.
// Refs: MGIT-174
func BaseCurrency(composedVersion, runningVersion string) Currency {
	if composedVersion == "" || runningVersion == "" {
		return CurrencyUnknown
	}
	if composedVersion == runningVersion {
		return CurrencyCurrent
	}
	return CurrencyStale
}
