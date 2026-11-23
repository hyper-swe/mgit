# MGIT-333.3: running a prepared review from the host (the contract)

This is the contract for the host verb that runs a review. It is written before the code; the first change that
implements it must match it, and a difference is a change to this page.

1. `mgit sandbox review run` runs one prepared review and returns what the reviewer produced. It takes the same
   identities as `review prepare` (profile, image, mapping, binding digest, bundle digest, nonce) in place of the
   bundle directory, plus `--lease <id>`, the lease the prepare step sealed. The caller asks for what it expects; the
   daemon checks that the sealed lease is exactly that.
2. What runs is never chosen by the caller. The program, its digest and its arguments come from the operator's
   mapping named by `--mapping`, and are checked against the attestation. The caller supplies no path, no arguments and
   no environment.
3. The verb refuses unless the lease verifies at run time, not only when it was sealed: the lease directory is private
   and holds exactly the evidence and the attestation, both read-only; the attestation verifies against the host key
   for the request, is unexpired and names this lease; the evidence still hashes to what was signed; and the seal in
   the guest took (root-owned, the reviewer's group, checked on disk). Any failure refuses before the reviewer exists.
4. The nonce is single use. The first run with a lease and nonce consumes it, before the guest is launched; a second
   run with the same nonce is refused with NONCE_REPLAYED and nothing starts. A refusal that happens before the
   consumption (an unknown lease, a request that does not match) does not consume it.
5. Two limits apply to every run. `--max-seconds` (default 600, at most 1800) is wall-clock, from the start of the
   reviewer. `--max-output-bytes` (default 4194304, at most 16777216) caps standard output and standard error together.
   Exceeding either stops the guest, discards the partial output and refuses with RUN_TIMEOUT or OUTPUT_LIMIT. These
   two have `reviewer_started` true in the envelope, because the reviewer did run.
6. Default output. Standard output is the reviewer's standard output, byte for byte. Standard error is the reviewer's
   standard error. The exit status is the reviewer's own, except that a reviewer exit of 70 or 71, which are reserved
   below, is reported as 1; `--format json` has the exact value.
7. `--format json` prints one object, `{"schema":"mgit.review-result/v1","lease":"...","exit_code":N,"signal":"",
   "stdout":"<base64>","stderr":"<base64>","elapsed_ms":N}`, and exits 0 whenever the reviewer ran to the end,
   whatever its own `exit_code`.
8. Refusals. A refusal prints exactly one `mgit.review-error/v1` envelope on standard error, prints nothing on standard
   output, and exits 70 when the reviewer did not start or 71 when it started and was stopped. The codes are the
   existing PROFILE_UNAVAILABLE, IMAGE_MISMATCH, MAPPING_MISMATCH, BUNDLE_INVALID, POLICY_UNENFORCEABLE and
   ATTESTATION_INVALID, and the new NONCE_REPLAYED (70), RUN_TIMEOUT (71) and OUTPUT_LIMIT (71). The envelope carries
   no bundle bytes, no prompt and no credential.
9. The guest is removed at the end of every run, whatever happened. The lease is left in place, marked used, for
   audit; it cannot be run again.
10. Not in this contract: egress and credentials (the attestation carries only their ids), a streaming mode, and any
    way to run without a prepared lease. A caller that needs the result as it is produced waits for a later change.
