# macOS release signing assets

## `vz.entitlements`

The entitlement that lets `mgit-sandboxd` drive Virtualization.framework
(`com.apple.security.virtualization`). The release job ad-hoc signs the daemon
with it (see `.goreleaser.yaml`, the `mgit-sandboxd-darwin` build's post hook):

```bash
codesign --force --sign - --entitlements build/darwin/vz.entitlements mgit-sandboxd
```

**Keep this file a bare plist — no XML comments.** `codesign` parses
entitlements with AMFI's strict XML reader, which rejects `<!-- ... -->` with
`AMFIUnserializeXML: syntax error`. Rationale for the entitlement choice lives
in `docs/INSTALL-SANDBOX.md`, not here.

Refs: MGIT-44, FR-17.15
