# Homebrew Tap Setup for mgit

## Changes needed in `hyper-swe/homebrew-tap`

### 1. Add Formula

Copy `brew/mgit.rb` to `Formula/mgit.rb` in the homebrew-tap repository. The SHA256 placeholders will be filled by the update workflow on first release.

### 2. Update the tap workflow

The existing `update-formula.yml` needs to handle mgit releases in addition to mtix. Add a new trigger type and a job for mgit:

```yaml
on:
  repository_dispatch:
    types: [release-published, mgit-release-published]
```

Add a condition to determine which formula to update based on `github.event.client_payload.project`.

### 3. Release trigger

mgit's release workflow (`.github/workflows/release.yml`) dispatches:
```
event-type: mgit-release-published
client-payload: {"tag": "v0.1.0", "project": "mgit"}
```

This requires the `HOMEBREW_TAP_TOKEN` secret to be set in the mgit repository with write access to `hyper-swe/homebrew-tap`.

### 4. Install command (after setup)

```bash
brew install hyper-swe/tap/mgit
```
