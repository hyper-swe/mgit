# Homebrew tap handoff — install both binaries

`hyper-swe/homebrew-tap` is a **separate repository** with its own
project-aware `update-formula` workflow (it keys off `client_payload.project`,
so mgit's release dispatch updates `Formula/mgit.rb` and never the `mtix`
formula). It cannot be edited from this repo, so this is the exact change the
tap owner must apply for MGIT-44.

## Required change

`Formula/mgit.rb` must install **both** host binaries on the platforms whose
release archive carries them (Linux any arch; macOS arm64), and install only
`mgit` where the archive is mgit-only (macOS amd64). The archive layout is:

- `mgit_<ver>_Linux_{amd64,arm64}.tar.gz` → `mgit`, `mgit-sandboxd`
- `mgit_<ver>_Darwin_arm64.tar.gz` → `mgit`, `mgit-sandboxd`
- `mgit_<ver>_Darwin_amd64.tar.gz` → `mgit` only

### If the formula is hand-maintained

Replace the single-binary install with a conditional one:

```ruby
def install
  bin.install "mgit"
  # mgit-sandboxd ships in the Linux and macOS-arm64 archives only.
  bin.install "mgit-sandboxd" if File.exist?("mgit-sandboxd")
end
```

`File.exist?` keeps one formula body correct across all four bottles: it
installs the daemon wherever the archive contains it and silently skips it on
Intel macOS, with no per-platform branching.

### If the formula is auto-generated

Update the tap's `update-formula` generator so, for `project == "mgit"`, the
generated `install` block is the conditional above (not a bare
`bin.install "mgit"`). Do **not** change the generation path for other projects
(the `mtix` formula must be untouched).

## Verification after the tap release

```bash
brew install hyper-swe/tap/mgit
command -v mgit
command -v mgit-sandboxd   # present on Linux and macOS arm64
```

On macOS arm64, confirm the bottle's daemon is entitlement-signed:

```bash
codesign --display --entitlements - "$(command -v mgit-sandboxd)" \
  | grep com.apple.security.virtualization
```

Refs: MGIT-44
