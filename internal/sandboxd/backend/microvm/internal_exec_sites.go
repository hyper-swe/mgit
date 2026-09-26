package microvm

// internalExecSite is one program the daemon runs inside a guest on its OWN
// initiative — not a client's command. This is the single source of truth for
// the daemon's internal execs (MGIT-272): the production code names these same
// programs (settleShell, guestProbeCommand), so the list below is their
// registry, not a copy of them.
//
// Every internal exec must hold two properties, which the enumeration test
// asserts for each entry: its program is named by an ABSOLUTE path, so nothing
// a guest resolves by name can stand in for it; and it either runs a real
// program through the AUDITED IDENTITY path (execSettler.run sets the identity
// and the sync records the privileged exec) or it runs no program at all (the
// readiness probe, whose reply proves the channel without a process, so there
// is no identity to govern and nothing to audit). Adding an internal exec
// means adding it here — and execSettler.run refuses a program that is not a
// registered audited site, so an unlisted one does not go unnoticed.
// Refs: MGIT-272, MGIT-151, FR-17.18
type internalExecSite struct {
	Name    string
	Program string // argv[0]; must be absolute
	// AuditedIdentity: runs a real program through execSettler.run (identity
	// set) and the sync's privileged-exec record. Exactly one of
	// AuditedIdentity or NoExec is true.
	AuditedIdentity bool
	// NoExec: names a program that cannot run (the readiness probe), so there
	// is no identity to set and nothing to audit.
	NoExec bool
}

// internalExecSites is every program the daemon runs inside a guest on its own
// initiative. Refs: MGIT-272
var internalExecSites = []internalExecSite{
	{Name: "settle read-back", Program: settleShell, AuditedIdentity: true},
	{Name: "readiness probe", Program: guestProbeCommand[0], NoExec: true},
}
