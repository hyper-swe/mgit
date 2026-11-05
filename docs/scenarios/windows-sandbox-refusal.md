# Sandbox launch on Windows reports an unavailable backend

1. In an initialized repository without a guest base, run
   `mgit sandbox launch --task-id T-1 --worktree <path>` on Windows, without
   `--image`.
2. The command reports that the sandbox is not available on Windows. It does
   not look up a guest base or contact a daemon. The error wraps
   `ErrSandboxBackendUnavailable`.
3. Run `mgit work <new-path> --task-id T-2 --sandbox`, also without `--image`.
   The worktree and routing setup still succeed. The sandbox launch reports
   the unavailable backend, not instructions to compose a guest base. The
   containment status says the sandbox is unavailable on this platform; it does
   not tell the user to launch it there.
4. Commands that connect to the sandbox daemon also report the unavailable
   backend. Repository, image, base and daemon-management commands are not
   covered by this refusal. Version control commands remain available.
5. Linux and macOS behavior is unchanged.

Checked through the production command tree by
`TestSandboxWindowsCommands_NoGuestBase`, with the host platform set to Windows
in the test. This is not a test on a Windows host.
