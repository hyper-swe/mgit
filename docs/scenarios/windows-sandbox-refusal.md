# The sandbox commands on Windows say they are not available yet

1. A user runs `mgit sandbox launch --task-id T-1` on Windows.
2. Before: the command looked for the repository, then for `mgit-sandboxd`, and
   failed with "mgit-sandboxd binary not found". That reads as a broken install.
3. After: the command stops first and says the sandbox is not available on
   Windows yet, that mgit has no sandbox backend for Windows, that version
   control commands work, that sandboxed work needs Linux or macOS (Apple
   silicon), that untrusted code must not be run directly on the host instead,
   and where the plan is (`docs/adr/017-windows-wsl2-sandbox.md`).
4. The error wraps `ErrSandboxBackendUnavailable`, so it is classified like any
   other missing backend.
5. Linux and macOS are unchanged.

Checked by `TestSandboxPlatformRefusal_ByPlatform` and
`TestSandboxConnectFor_OnWindows_ReportsUnavailableBeforeAnythingElse`.
