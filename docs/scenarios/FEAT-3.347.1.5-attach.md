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

This daemon wires the map store and the binding but no credential source or
egress enforcer, so scenarios 2 and 4 run against fakes only.
