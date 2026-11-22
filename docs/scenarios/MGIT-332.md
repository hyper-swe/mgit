# MGIT-332: a review launcher that runs one pinned program and nothing else

1. A review is to run one operator-pinned program inside a guest, with no way for it to start another.
   `mgit-guest review-exec --sha256 <digest> [--uid N] -- <entrypoint> [args...]` is the launcher.
2. The launcher opens the entrypoint, hashes the bytes of that open file against the pinned digest, and
   refuses a mismatch, a malformed digest, or a file that is not an ELF executable (a script would need an
   interpreter, which is a second launch). Refusal exits 70 with a neutral error envelope on stderr and the
   program is not started.
3. A launcher that starts as root refuses unless it is given an unprivileged identity; with one it clears the
   capability bounding set and ambient capabilities, drops groups, gid and uid. An identity with a uid or gid of zero (or below) is
   refused too, whoever starts the launcher.
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
8. The profile also refuses file changes: every call that creates, changes or removes a file, directory, link,
   attribute or mount fails with "operation not permitted", an open for writing, creating, truncating or
   appending fails the same way (read-only opens work), and the calls that would route around that (a rewritten
   open, a ring submission, the mount calls) are refused or answered "not implemented". The program starts
   with no descriptor beyond the standard streams except read-only ones and the runtime's own. Standard output
   and error still work.
9. Syscalls the profile does not know yet answer "not implemented", so a call the kernel adds later is not
   allowed by omission. `ioctl` is an allowlist of terminal and descriptor requests, so a read-only descriptor
   cannot be used to change a file's flags or attributes. Message queues, System V shared memory, queues and
   semaphores, and the descriptor-taking process calls are refused. A descriptor the caller left open is
   closed to the program, including a writable or append-mode one.
10. On a kernel older than 5.11, which cannot mark a whole range of descriptors at once, the launcher marks them
    one by one from the directory of open descriptors. If neither works, the launcher refuses with
    POLICY_UNENFORCEABLE and starts nothing.
11. Left open on purpose: writing to a descriptor the caller handed in as standard output or error, and
   connecting to a unix socket the user may already open; both are decisions of the caller and the guest's
   network policy, not of the launcher.
12. Not here: the sealed read-only bundle, the signed attestation, leases, egress and credentials (later
   slices), and an arm64 run (the filter table is tested for arm64; the guest run is amd64).
