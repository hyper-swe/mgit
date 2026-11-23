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
4. The nonce is single use, and its scope is the lease: the first run of a lease consumes it, before the guest is
   launched, by creating a durable record that only one of any number of concurrent requests can create. The rest, and
   any later run of that lease, are refused with NONCE_REPLAYED and start nothing. A refusal that happens before the
   consumption (an unknown or expired lease, a request that does not match, changed evidence, a changed mapping) does not
   consume it, so a corrected request may follow. Everything after the consumption leaves the lease spent, even when
   the reviewer never started (a guest that would not launch, a seal that did not take): the caller prepares again.
5. Two limits apply to every run. `--max-seconds` (0 or absent means 600; otherwise 1 to 1800) is wall-clock from the
   start of the reviewer. `--max-output-bytes` (0 or absent means 4194304; otherwise 1 to 16777216) counts the raw bytes
   of standard output and standard error together, before any encoding. A negative value or one above the maximum is
   refused at the protocol boundary as a malformed request, with no envelope and no consumption. Output of exactly the
   cap succeeds; one byte more is refused with OUTPUT_LIMIT and no byte of either stream is returned. A run that exceeds
   its time is refused with RUN_TIMEOUT. If both would apply, RUN_TIMEOUT wins, since an unfinished run has no complete
   output to measure. Both refusals mean the reviewer ran, and the guest is stopped and removed.
6. Default output. Standard output is the reviewer's standard output, byte for byte. Standard error is the reviewer's
   standard error. The exit status is the reviewer's, except that a reviewer exit of 70 or 71, which are reserved
   below, is reported as 1; `--format json` has the exact value. A reviewer killed by signal n has the status 128+n,
   as a shell reports it (for example 137 for a kill), in both modes; the text mode exits with it and the JSON shows it
   as `exit_code`. The `signal` field of the JSON result is empty in this version: the guest does not report a signal
   separately, so a reviewer that exits 137 by itself cannot be told apart from one that was killed.
7. `--format json` prints one object, `{"schema":"mgit.review-result/v1","lease":"...","exit_code":N,"signal":"",
   "stdout":"<base64>","stderr":"<base64>","elapsed_ms":N}`, and exits 0 whenever the reviewer ran to the end,
   whatever its own `exit_code`.
8. Refusals. A refusal prints exactly one `mgit.review-error/v1` envelope on standard error, prints nothing on standard
   output, and exits 70 when the reviewer did not start or 71 when it started and was stopped. The envelope carries no
   bundle bytes, no prompt and no credential. The codes:

   | situation | code | reviewer_started | exit | nonce still usable |
   | --- | --- | --- | --- | --- |
   | profile not available, or no guest backend | PROFILE_UNAVAILABLE | false | 70 | yes |
   | unknown lease, or a lease directory that is not the sealed shape | BUNDLE_INVALID | false | 70 | yes |
   | request does not match the attestation (nonce, binding, image, bundle digest), expired attestation, changed evidence | ATTESTATION_INVALID | false | 70 | yes |
   | mapping unknown, or changed since prepare | MAPPING_MISMATCH | false | 70 | yes |
   | backend does not deliver the bundle as a launch-time image | POLICY_UNENFORCEABLE | false | 70 | yes |
   | lease already run, or lost a concurrent race | NONCE_REPLAYED | false | 70 | no (already spent) |
   | the guest could not be launched or its seal did not take | POLICY_UNENFORCEABLE | false | 70 | no (spent) |
   | the bindings refused (a credential or network policy that is not provided) | POLICY_UNENFORCEABLE | false | 70 | no (spent) |
   | the launcher declined (the program is not the pinned one, and so on) | the launcher's code | false | 70 | no (spent) |
   | the reviewer ran past its time | RUN_TIMEOUT | true | 71 | no (spent) |
   | the reviewer produced more than the output limit | OUTPUT_LIMIT | true | 71 | no (spent) |
   | the run could not be taken to its end for another reason after the reviewer may have started | POLICY_UNENFORCEABLE | true | 71 | no (spent) |

   A request that is malformed at the protocol boundary (a missing field, a lease that is not 32 lower-case hex
   digits, a limit out of range) is refused as an ordinary error with exit 1 and no envelope.
9. The guest is removed at the end of every run that launched one, whatever happened. The lease is left in place,
   marked used, for audit, if and only if the nonce was consumed; it cannot be run again.
10. Execution is tied to what was checked. The program, its digest and its arguments are those of the operator's
    mapping, which is checked against the attestation at run time (the same id, binding, image, entrypoint digest,
    arguments, network policy and credential reference). The guest is built from the lease as it was verified, and the
    in-guest seal checks the evidence's digest against the attestation before it hands anything to the reviewer, so a
    change to the lease between the host's check and the guest's image is refused as a failed seal.
11. A connection that ends before the caller has a result tells the caller nothing about whether the daemon had consumed
    the lease, or whether the reviewer started: it is reported as an ordinary failure (exit 1, no envelope), never as a
    refusal, and the caller must not assume that nothing ran. The guarantee is at most one run per lease, not that every
    retry is refused. Two cases follow:
    - The request never reached the daemon, or the caller was gone before the daemon consumed the nonce (the daemon
      checks for this just before consuming): nothing is spent, and a retry of the same lease is the first run.
    - The nonce had been consumed: the lease is spent, the guest is removed, and a retry is refused with NONCE_REPLAYED;
      it never launches the review again. This verb stores no result, so a caller that lost one prepares a new lease.
    A guest does not outlive a daemon that stops mid-run: its virtual machine monitor dies with the daemon (a
    parent-death signal), and a graceful stop removes the guests first. The per-guest files the backend keeps are the
    backend's own to clear, not this verb's.
12. What a review needs beyond the sealed bundle (the binding map, a network policy, a credential) is installed behind
    one interface, `reviewsvc.Bindings`, called once per run after the guest is sealed and before the reviewer starts,
    and released whatever happens. The default installs nothing and grants nothing: a mapping that names a credential
    or any network policy other than none is refused with POLICY_UNENFORCEABLE. See MGIT-333.4.md.
13. Not in this contract: egress and credentials (the attestation carries only their ids), a streaming mode, and any
    way to run without a prepared lease. A caller that needs the result as it is produced waits for a later change.
