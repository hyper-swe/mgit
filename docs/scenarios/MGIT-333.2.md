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
5. The reviewer identity must have a uid and gid above zero; otherwise the guest is not launched (MAPPING_MISMATCH).
   The program is only ever started as `mgit-guest review-exec --sha256 <digest> --uid N --gid N --dir <lease> --
   <path> <argv...>`, with an empty environment. The digest must be 64 lower-case hex digits and the path absolute, or
   the run is refused with MAPPING_MISMATCH and nothing reaches the guest.
6. A launcher refusal (exit 70 and the `mgit.review-error/v1` envelope on stderr) comes back as that error, with the
   exit status and output kept. Any other exit status is the reviewer's own.
7. Before: there was no way to start a guest for a review, so the launcher and the sealed bundle had no host-side
   caller. After: they have one, and a backend that could carry a write back is refused up front.
8. On the firecracker live job, a real guest is provisioned through `Provision`: the same program, as the same user, creates
   a file when run directly and cannot when run through the launcher, and a wrong digest is refused with HASH_MISMATCH.
9. The lease is handed to the reviewer without the reviewer owning any of it. After launch, as root in the guest,
   `mgit-guest review-seal --gid <reviewer gid> <lease>` makes the directory and its files root-owned with the
   reviewer's group, the directory 0750 and the files 0440, then looks at what is on disk and fails unless every
   entry is root-owned with that group and has the mode the seal sets. A directory holding anything but regular files is refused, except the `lost+found` directory every filesystem image carries, which is left root-owned 0700. The exemption is exact: a real directory with that name at the lease root, root-owned, empty and mode 0700; a file, a link, a non-empty one or one anywhere else is refused. The reviewer reads
   through its group and can change no mode or owner, so this holds even when its uid equals the uid the daemon
   delivered the files as. A seal that fails removes the guest and refuses with POLICY_UNENFORCEABLE before any reviewer runs. This needs the seal
   step to run as root in the guest, which a guest launched by the daemon does.
10. Not here: the review-run verb and leases (the next slice), the binding-map installer, egress and credentials. The
   box guest proof, with a marker-file control, is recorded in the pull request.
