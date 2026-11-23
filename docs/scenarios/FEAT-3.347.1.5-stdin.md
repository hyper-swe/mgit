# FEAT-3.347.1.5: a small secret reaches a guest process on its standard input

1. `model.ExecRequest` has a `Stdin` field of type `model.Secret`. It has no JSON name, so it is not part of the control
   protocol: a local client cannot send it (a request that names `stdin` is refused, the wire shape and the protocol
   version are unchanged), and it is absent from every audit record. Only the daemon's own code can set it.
2. Between the daemon and the guest the bytes travel in the exec request frame as a separate key, not in argv or the
   environment. The guest pipes them to the child's standard input and closes it. A request with no stdin is encoded as it
   always was.
3. Stdin is bounded at 4096 bytes: the request is refused by `Validate`, and the guest refuses a larger frame.
4. `model.Secret` renders as `[redacted]` under every format verb, as JSON, as text and as a structured-log value; the
   guest's own log of an exec carries no stdin.
5. `mgit-guest cred-install --dest /run/review-cred/<name> --gid <n>` reads the credential from stdin and writes it to a
   new file, mode 0440, owned by root and the given group. It refuses an empty or oversized stdin (so a guest that dropped
   stdin cannot install an empty slot), a destination that is not one plain name directly inside the credential directory,
   group zero, and a slot that already exists. It prints nothing of the credential.
6. `mgit-guest cred-remove --dest <path>` overwrites the file with zeros and unlinks it; an absent slot is not an error,
   so a release can run twice.
