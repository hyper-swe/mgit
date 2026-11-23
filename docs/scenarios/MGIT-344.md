# MGIT-344: a review whose mapping names an egress policy

1. An operator installs a review mapping that names an egress policy, on a Linux
   host where the daemon has its host egress stack (the allowlist proxy and DNS).
2. Before: every review guest launched with no network, by a path that never started
   the host stack, so a grant for the guest failed and such a review could not run.
3. After: the review guest launches in allowlist mode with nothing on the list, and
   the host stack is up before anything runs in the guest, the seal included.
4. The bindings are given a grant function. Each grant admits one exact ip:port,
   never a host name, before the reviewer starts. A grant the host does not admit
   refuses the review before it starts.
5. When the run ends, every grant is revoked and the stack is stopped before the
   guest is removed, whether the run succeeded, failed or was refused.
6. A mapping with no egress policy, or `none`, still gets a guest with no network,
   and no grant function. So does every review where the daemon has no host egress
   stack, which is every build other than Linux. There, a binding that needs egress
   refuses rather than running without it.
7. If the stack does not start, the guest is removed and the review is refused
   before the reviewer starts.
