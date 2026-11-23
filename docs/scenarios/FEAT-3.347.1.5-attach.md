# FEAT-3.347.1.5 — attaching the binding to a review run

The review run asks its bindings, after the guest is sealed and before the
reviewer starts, to install what the operator's map names.

## Scenarios

1. A map that names no credential and egress `none` installs nothing; the
   release function is non-nil and safe to call twice.
2. A map with a credential and a named host: the egress policy is applied, the
   credential is read once from the operator's source and handed to the guest
   verb on standard input. It never appears in a command line or environment.
   Release removes the file and lifts the policy, once.
3. A host policy with no enforcer, or a credential with no source, is refused
   as POLICY_UNENFORCEABLE before anything is installed.
4. A failed install lifts the egress policy and still returns a usable release;
   no error carries the credential.

Host egress: a map whose egress policy is `host:<fqdn>` is granted through the daemon's egress stack for that review guest only.
The host is resolved on the host side; every resolved address is admitted on port 443 and no other port, and nothing else is
reachable (an address off the grant, or a port off it, is not reachable). The addresses are fixed at grant time, so the grant does not
follow later DNS changes. Every grant is revoked when the guest is removed. A guest with no egress stack (a daemon without one,
or a policy of `none`) refuses a `host:` map before anything is installed.

The credential source is optional (`--review-credential-fd`). The attach tests run against fakes; the live off-grant address and
port cases are covered by the egress seam's own firecracker e2e.
5. A release whose cleanup exits nonzero, or returns no result, reports an error that carries no credential bytes.
