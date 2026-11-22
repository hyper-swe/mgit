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
   (detached guest, sealed read-only bundle, no writes, no further exec, no host mounts). Egress policy and
   credential reference are ids only; no secret value exists in it.
4. `Verify` is the client side: it refuses a wrong key, a changed field, an expired attestation, any
   requested identity that differs, or an enforced block weaker than the profile.
5. Before: nothing checked a review request or attested a launch.
6. After: both directions are table-tested, every signed field is covered by a tamper case.
7. Not here (later slices): the wire verb and CLI, the guest kind and bundle delivery (R2b), lease use at run,
   the binding map loader, egress and credentials (R3, R4).
