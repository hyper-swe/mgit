# MGIT-332: a review launcher that runs one pinned program and nothing else

1. A review is to run one operator-pinned program inside a guest, with no way for it to start another.
   `mgit-guest review-exec --sha256 <digest> [--uid N] -- <entrypoint> [args...]` is the launcher.
2. The launcher opens the entrypoint, hashes the bytes of that open file against the pinned digest, and
   refuses a mismatch, a malformed digest, or a file that is not an ELF executable (a script would need an
   interpreter, which is a second launch). Refusal exits 70 with a neutral error envelope on stderr and the
   program is not started.
3. A launcher that starts as root refuses unless it is given an unprivileged identity; with one it clears the
   capability bounding set and ambient capabilities, drops groups, gid and uid.
4. It sets no_new_privs and a seccomp filter, then executes the verified file by descriptor. The filter
   refuses execve by path, fork and vfork; allows clone only as a thread; answers clone3 with ENOSYS so C
   libraries fall back to clone; and sends execveat to a supervisor that allows exactly one: the launcher's
   own, on that descriptor with AT_EMPTY_PATH.
5. Before: nothing prevented the program from starting a shell, a second program or a child.
6. After: the program runs, its threads work, and a path exec, a fork and an exec by descriptor each fail with
   "operation not permitted". The program sees an empty environment.
7. The launcher process, which judges the one allowed exec, makes itself non-dumpable before it starts the
   program, so a same-user program cannot reach its memory or descriptors through `/proc`. The guest's
   `ptrace_scope` is readable (`YamaScope`) so a receipt can record it.
8. Not here: the sealed read-only bundle, the signed attestation, leases, egress and credentials (later
   slices), and an arm64 run (the filter table is tested for arm64; the guest run is amd64).
