# MGIT-333.2: a review guest that sees only the sealed bundle and starts only the pinned program

1. A review runs in a guest that holds the sealed lease directory and nothing else of the host. `reviewguest.Provision`
   launches it; `Guest.Run` starts the mapped program in it; `Guest.Close` removes it.
2. The guest is launched on the lease directory with no network, no repository, no published ports and no confined
   agent topology, from an image pinned by digest. An image that is not pinned is refused with IMAGE_MISMATCH before
   anything launches.
3. The bundle must reach the guest as an image built at launch, so nothing the guest writes can travel back to the
   host. Only the launch-time backend is accepted. The backends that share the host directory live (libkrun, vzf,
   container), the one not shown to copy (hyperv) and an unnamed one are refused with POLICY_UNENFORCEABLE before
   launch. If the manager then reports a different backend than was declared, the guest is removed and the same
   refusal is returned, so a reviewer is never run on a live share.
4. A lease directory that is not absolute, is missing, is a file or is a symbolic link is refused with
   BUNDLE_INVALID before launch.
5. The program is only ever started as `mgit-guest review-exec --sha256 <digest> --uid N --gid N --dir <lease> --
   <path> <argv...>`, with an empty environment. The digest must be 64 lower-case hex digits and the path absolute, or
   the run is refused with MAPPING_MISMATCH and nothing reaches the guest.
6. A launcher refusal (exit 70 and the `mgit.review-error/v1` envelope on stderr) comes back as that error, with the
   exit status and output kept. Any other exit status is the reviewer's own.
7. Before: there was no way to start a guest for a review, so the launcher and the sealed bundle had no host-side
   caller. After: they have one, and a backend that could carry a write back is refused up front.
8. Not here: the review-run verb and leases (the next slice), the binding-map installer, egress and credentials. The
   box guest proof, with a marker-file control, is recorded in the pull request.
