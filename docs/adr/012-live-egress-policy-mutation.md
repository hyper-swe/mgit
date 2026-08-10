# ADR-012: Live egress policy mutation (kill established flows by default)

**Status:** Accepted
**Date:** 2026-08-09
**Refs:** MGIT-72, MGIT-74, FR-17.8, FR-17.12, FR-17.18, SEC-04, SEC-05, ADR-005, ADR-010

## Context

HyperSwe's provisioning design grants package-registry egress while a task's
environment is being set up (`npm install`, `pip install`, `apt`), and revokes
it before the untrusted dev/test run. That is the correct shape: the window in
which arbitrary code can reach the network should be the setup window, not the
whole task.

mgit could not express it. The only way to change a sandbox's egress policy was
to relaunch the sandbox — which destroys the environment that was just
provisioned. So callers ran with "held-for-the-run" egress and disclosed it,
a weaker posture than their design intended.

The change is tractable because the enforcement point is per connection.
`netgw.go`'s `handleForward` consults the authorizer for **every** inbound
connection request before completing the handshake, and the firecracker
transparent proxy does the equivalent. Swap the compiled ruleset and the very
next flow is decided against the new policy, with no VM involvement at all.
This is a control-plane change, not a data-path one.

What is *not* settled by that observation is what happens to connections that
are already open. That is the decision this ADR records.

## Decision

### 1. Established flows are TERMINATED by default; `--drain` is opt-in

A revoke closes every flow the sandbox currently has spliced, unless the caller
asks for draining by name.

Firewalls conventionally do the opposite — existing connections drain, new ones
are refused — so this is a deliberate departure and it needs its reason stated:

- The caller's next action is *running untrusted code*. That is the entire
  reason they revoked. A draining connection is precisely the exfiltration
  channel they just revoked, still open, still writable.
- A hostile guest chooses how long its connections live. Against a guest that
  simply never closes its socket, "drain" means "never". A guarantee an
  adversary can veto is not a guarantee.
- The failure is silent. Nothing in the caller's environment reports that one
  connection outlived the revoke, so the weaker behavior would be invisible
  exactly when it mattered.

Draining remains available because there are legitimate uses (letting an
in-progress large download finish before closing the window), but it has to be
requested: `--drain` on the CLI, `drain: true` on the MCP tool. The surface
states the default at the verb, in the help text of every command that can
terminate a connection, because kill and drain are opposite security postures
and a caller who assumes the other one is exposed.

### 2. Revoke is an empty set, not a separate code path

`policy set` with no entries and `policy revoke` are the same operation
underneath: compile a replacement ruleset and swap it in. One code path means
revoke cannot rot into a no-op while set keeps working.

The CLI and MCP surfaces nonetheless **refuse** an empty `set`, directing the
caller to `revoke`. The operations are identical; the *intents* are not, and a
caller who meant to grant and mistyped the flag would otherwise silently revoke
everything.

### 3. The swap is atomic; a policy that does not compile changes nothing

The replacement is compiled **before** the lock is taken and installed in one
assignment. A concurrent authorization decision therefore sees the old ruleset
or the new one and never a mixture. A malformed policy is rejected with the
running one untouched — a half-applied policy is the one outcome a caller can
neither predict nor audit.

### 4. Live capability grants do not survive a policy change

A capability grant (FR-17.12) widens the policy it was approved under. Carrying
it across a replacement would leave a hole that is invisible in the new policy.
Grants are dropped on every mutation; a caller who still wants one re-grants it
against the new rules.

### 5. Every mutation is an append-only audit event, recorded at both ends

`policy_changed` is an audit-only sandbox event (no lifecycle state change — a
revoke does not suspend or destroy a sandbox). Each mutation appends two
records:

- `phase: "requested"` **before** the change. No widening can take effect
  unaudited, and an audit sink that cannot be written **blocks** the mutation.
- `phase: "applied"` after, carrying the entries now in force, the rule count,
  and `established_flows_killed` — or `phase: "failed"` with the error.

Both ends are recorded on purpose. A trail of intentions alone would claim a
policy changed when the enforcer refused it; a trail of outcomes alone would
let an unrecorded widening take effect. `established_flows_killed` is the
number that carries "revoke means revoke", so it is on the record and not only
in the reply.

Reads (`policy show`) are **not** audited. A trail padded with reads is one
nobody reviews.

### 6. Only the host may mutate policy

Every entry point is host-side, and this is structural rather than a check:

- On firecracker the enforcer is the daemon's own egress runner, in the daemon
  process. The guest has no channel to it.
- On libkrun the enforcer lives in a re-exec'd VM child (ADR-010), reached over
  a host-initiated control socket in the sandbox **state** dir. The guest's
  share is the worktree-staging subdirectory *inside* that dir, so the guest
  has no filesystem path to the socket at all.

The guest's three channels — exec, land, and the egress data path — carry no
policy verb. SEC-05 holds after adding a way to mutate policy at runtime
because there is no route from the guest to any of it.

### 7. The live policy is readable

`policy show` reports what is being enforced **now**. Without it the only
readable policy is the launch-time one on `mgit sandbox status`, which a
mutation makes wrong — and a revoke a caller cannot confirm is a revoke taken
on faith. An unknown or non-enforcing sandbox is an **error**, never an empty
policy: "nothing is allowed" and "nothing is enforcing" look identical in an
empty list and are opposite facts.

## Consequences

**Good**

- Provisioning gets the posture it designed for: egress during setup, none
  during the untrusted run, with the environment preserved.
- The strong reading of "revoke" is the default, and the weak one is visible in
  the command line of anyone who chose it.
- A reviewer can reconstruct every policy change, its outcome, and what it
  terminated, from the append-only trail.

**Costs**

- A caller who *wanted* firewall-conventional draining gets connection resets
  until they learn the flag. This is a deliberate trade: the surprising default
  is the safe one, and it is documented at the verb.
- Two audit records per mutation instead of one.
- Backends must each provide an enforcer adapter; a build with none reports the
  verbs unserved rather than pretending.

**Live-VM evidence (MGIT-72)**

On **libkrun** the kill path is proven on hardware, not only in unit tests: a
probe holds a connection open across the revoke and reports whether it survived
(`TestE2E_Libkrun_RealVM_Revoke_{Kills,DrainKeeps}EstablishedFlow`, re-run on
Apple Silicon 2026-08-10 against a real off-host destination):

- kill (default): host reports `killed=1`, guest reports
  `PROBE-RESULT HOLD = DIED after=0s reason="EOF"`.
- drain (`--drain`): host reports `killed=0 drained=true`, guest reports
  `PROBE-RESULT HOLD = SURVIVED after=10.11s`.

Both additionally assert the **next** flow is refused by policy, so "survived"
can never be a mutation that silently did nothing. The drain case is the
positive control that makes the kill case a decision rather than a coincidence.

On **firecracker** the same two assertions exist as
`TestE2E_Firecracker_Revoke_{Kills,DrainKeeps}EstablishedFlow`, and as of
**2026-08-10 (MGIT-78) they are proven on real KVM hardware too**. This section
previously read "not been run on hardware yet … *implemented and reviewed, not
proven*", because the pair needs a Linux KVM host with a guest image and
net-root and skipped everywhere else. That is no longer the state: they now run
on every push, PR and release, under `sudo -E`, against a real firecracker
microVM — `e2e.yml`'s `sandbox-live-linux` job, on a hosted `ubuntu-latest`
runner, which does expose `/dev/kvm`. The job names both tests explicitly and
fails if either merely skips, because a skip is what would otherwise turn this
paragraph back into a claim nobody checked.

Recorded output (GitHub Actions run 31381085771, `--- PASS` for both):

- kill (default): host reports
  `revoke applied: rules=0 killed=1 drained=false`; the guest probe reports
  `PROBE-HOLD ESTABLISHED bytes=40 head="REAL-BYTES-FROM-ALLOWLISTED-DESTINATION"`
  then `PROBE-RESULT HOLD = DIED after=0s reason="EOF"` —
  *"REAL VM PASS (firecracker kill): an established, data-carrying flow was
  TERMINATED by the revoke (host killed=1) and the next flow was refused"*.
- drain (`--drain`): host reports
  `drained revoke applied: rules=0 killed=0 drained=true`; the same probe
  reports `PROBE-RESULT HOLD = SURVIVED after=20.042s` —
  *"REAL VM PASS (firecracker drain control): the SAME held flow survived the
  SAME revoke under drain (killed=0) while the next flow was refused"*.
- In both cases the next flow is refused **by policy**:
  `post-revoke flow refused (backend observable: "PROBE-RESULT DIAL =
  CONNECTED-NO-DATA reason=\"read tcp …->140.82.112.3:443: read: connection
  reset by peer\"")`. That is the connect-then-reset this design predicted for
  this backend, and it is materially different from libkrun's connect-refused:
  the guest's SYN is REDIRECTed to the host proxy, so the handshake completes
  and the proxy then resets. The assertion is on the *class* of outcome —
  reached the enforcement point, carried no bytes — and explicitly fails on
  "network is unreachable"/"no route to host", so a revoke that broke the stack
  could never pass as a revoke that refused the flow.

The two backends still enforce by different mechanisms — a userspace netstack in
a re-exec'd VM child vs. tap + iptables REDIRECT into a host-side proxy — so
neither result was ever evidence for the other. Both are now measured
separately, which is exactly what that split demanded.

What was already true on firecracker by construction, and unit-tested: its
transparent proxy splices through `SpliceTracked`, so established flows land in
the same `FlowRegistry` the revoke closes, and `Runner.SetPolicy` fails closed on
a sandbox with no running egress stack. The verb therefore either enforces or
errors — the behavior ruled out on every backend is answering a revoke with
success while the old policy stays in force.

## Alternatives considered

**Drain by default, `--kill` to opt in.** Matches firewall convention and would
surprise fewer people. Rejected: the default must be the safe one, and here the
unsafe reading is silent, adversary-controlled, and arrives at the exact moment
untrusted code starts running.

**No default — require an explicit flag every time.** Considered seriously,
since it makes the choice impossible to get wrong by omission. Rejected because
it makes the common, correct call more awkward than the rare one, and because
an agent caller generating the command would have to encode a choice it has no
basis to make.

**Kill only flows to destinations the new policy removes.** Appealing and
strictly less disruptive. Rejected for v1: the flow registry tracks spliced
connections, not the rule each was authorized against, and a partial teardown
whose completeness depends on rule-matching is far harder to prove correct than
"close everything". The conservative behavior is also the safe one here.

**Relaunch (the status quo).** Correct but destroys the provisioned
environment, which is what made callers hold egress open for the whole run.
