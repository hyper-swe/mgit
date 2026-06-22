package model

// T2 — Fully confined agent (ADR-005). An opt-in hardened topology where
// the guest image bundles the agent CLI and the user attaches via
// `mgit sandbox shell`. Credentials are injected per attached session and
// never baked into the image; the injection is flagged in the audit log
// without the secret value. Default topology is T1 (confine_agent off).
// Refs: FR-17, ADR-005, MGIT-11.11.4

// SessionCredential is one secret injected into a confined-agent guest for
// the lifetime of a single attached session. The Value is NEVER serialized
// (json:"-"), so it cannot leak into the audit log, the guest image config,
// or any other persisted artifact — only the Name is ever recorded.
// Refs: MGIT-11.11.4
type SessionCredential struct {
	Name  string `json:"name"`
	Value string `json:"-"` // secret: never serialized, never audited
}

// RedactedCredentialNames projects credentials to their names only, for the
// audit record that flags THAT credentials were injected without disclosing
// WHAT they were. Refs: MGIT-11.11.4
func RedactedCredentialNames(creds []SessionCredential) []string {
	names := make([]string, 0, len(creds))
	for _, c := range creds {
		names = append(names, c.Name)
	}
	return names
}
