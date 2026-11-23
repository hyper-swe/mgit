# MGIT-333: review prepare checks a request and signs exactly what it will run

1. A caller asks for a review of one bundle with one pinned program. Before anything starts, the host checks
   the request against an operator-installed binding map and the bundle bytes, and returns a signed
   `mgit.review-attest/v1` stating the identities and the policy that will hold.
2. `Prepare` refuses, with `reviewer_started=false` and a neutral `mgit.review-error/v1` envelope, an unknown
   profile (PROFILE_UNAVAILABLE), an image digest that is missing or differs from the map (IMAGE_MISMATCH), a
   mapping id, map schema or binding digest that differs (MAPPING_MISMATCH), bundle bytes that are empty, not
   JSON or not the declared digest (BUNDLE_INVALID), an entrypoint that is not a pinned absolute clean path, or
   a credential with no declared egress policy (POLICY_UNENFORCEABLE), and a missing or short nonce
   (ATTESTATION_INVALID). No attestation is returned on a refusal; the envelope never carries bundle bytes.
3. On success the attestation carries nonce, lease, expiry, profile, image, mapping, binding, entrypoint,
   canonical argv and exact bundle digests, cwd `/review`, the capability policy digest and the enforced block
   (detached guest, sealed read-only bundle, whether file writes are denied, no further exec, no host mounts). Egress policy and
   credential reference are ids only; no secret value exists in it.
4. `Verify` is the client side: it refuses a wrong key, a changed field, an expired attestation, any
   requested identity that differs, or an enforced block weaker than the profile.
5. Before: nothing checked a review request or attested a launch.
6. After: both directions are table-tested, every signed field is covered by a tamper case. The capability policy
   text, and so its digest, states writes as denied, because the launcher this build ships denies them. A build
   whose launcher does not would say not denied, and a client that requires the profile would refuse it.
7. `mgit sandbox review prepare --profile reviewer-ro/v1 --image <name>@sha256:<digest> --mapping <id>
   --binding-digest <sha256> --bundle <dir> --bundle-digest <sha256> --nonce <random> --format json` asks the
   daemon for this. Success prints only the attestation on standard output. A refusal prints the error envelope
   on standard error, nothing on standard output, and exits nonzero. Nothing is started.
8. The daemon reads the operator-installed map from its own host state, never from the bundle. The bundle is a
   directory that holds exactly one regular file, `evidence.json`; a link, an extra file or a missing file is
   refused, so no project instruction file rides along. On success the evidence and the attestation are sealed
   read-only under a fresh lease, and what is later run is that sealed copy, not the source. A refusal leaves
   no lease behind. A daemon without the host signing key does not serve the verb.
9. The attestation is short-lived (at most 15 minutes), the evidence is one JSON object of at most 1 MiB (a
   scalar, an array or null is refused), and the launch always goes through the launcher with the attested
   digest, since only the digest of the entrypoint is signed, not its path.
10. The attestation reports only what the launcher enforces: until the launcher denies file changes, the
    attestation states writes as possible and a client that requires the profile refuses it.
11. The control protocol is now version 7, so an older daemon is refused at the handshake instead of answering
    "invalid request".
12. Not here (later slices): the guest kind and bundle delivery into a guest, lease use at run, the binding map
    installer, egress and credentials (R2c, R3, R4).
