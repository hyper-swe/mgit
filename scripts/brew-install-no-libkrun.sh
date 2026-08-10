#!/usr/bin/env bash
#
# Proves `brew install <tap>/mgit` works on a macOS machine that does NOT have
# libkrun. This is the MGIT-75 regression guard, and it is deliberately built
# so that it CANNOT pass for the wrong reason.
#
# The bug: brew/mgit.rb declared `depends_on "libkrun/krun/libkrun"`. Homebrew
# resolves dependencies before it fetches anything and refuses to LOAD a
# formula from an untrusted third-party tap, so the install aborted with exit
# 1 having installed nothing at all -- not even core mgit, which is CGO-free
# and never links libkrun.
#
# Why this script exists rather than an inline `run:` block: the bug is
# invisible on any machine that already has libkrun, because a satisfied
# dependency never has to be loaded. Every machine that has ever developed or
# tested mgit's sandbox is in that state, including the one that found the
# bug. Keeping the check in a file means the local reproduction and the CI job
# run the SAME code -- a developer can point it at a scratch Homebrew prefix
# via $BREW and get the runner's answer, not a false green.
#
# Usage:
#   scripts/brew-install-no-libkrun.sh              # uses `brew` from PATH
#   BREW=/path/to/scratch/brew scripts/brew-install-no-libkrun.sh
#
# Refs: MGIT-75, MGIT-69, MGIT-70

set -euo pipefail

BREW="${BREW:-brew}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAP_USER="mgit75check"
TAP_NAME="local"
TAP="${TAP_USER}/${TAP_NAME}"
FORMULA="${TAP}/mgit"
# A version string with no leading zeros or dots to trip over; the formula's
# own `test do` block asserts `mgit --version` reports it, so the binary is
# stamped with the same value below.
VERSION="0.0.0-mgit75check"

# Every step is `|| true`: this runs on EXIT, including the failure exits, and
# a cleanup step that fails must not overwrite the status the check produced.
cleanup() {
  "$BREW" uninstall --force "$FORMULA" >/dev/null 2>&1 || true
  "$BREW" untap "$TAP" >/dev/null 2>&1 || true
  if [ -n "${WORK:-}" ]; then rm -rf "$WORK" || true; fi
}
trap cleanup EXIT

# --------------------------------------------------------------------------
# 1. PRECONDITION: prove libkrun is absent.
#
# This runs FIRST and is fatal. A green install below means nothing unless
# the dependency actually had to be resolved, and it only has to be resolved
# when it is not already satisfied. Asserting the precondition is the entire
# difference between this check and the dry-run that passed while the bug was
# live.
# --------------------------------------------------------------------------
echo "==> Precondition: libkrun must be absent"
if "$BREW" list --versions libkrun >/dev/null 2>&1; then
  echo "FAIL: libkrun is already installed on this machine." >&2
  echo "      This check cannot prove anything here: a satisfied dependency is" >&2
  echo "      never loaded, so the failure it guards against is invisible." >&2
  echo "      Run it on a runner or Homebrew prefix without libkrun." >&2
  exit 1
fi
if "$BREW" --prefix libkrun >/dev/null 2>&1; then
  echo "FAIL: a libkrun prefix exists even though the formula is not installed." >&2
  exit 1
fi
echo "    ok: \`brew list --versions libkrun\` and \`brew --prefix libkrun\` both fail"

# The other half of the precondition: the gate has to be armed. If the
# libkrun/krun tap were already trusted here, a reintroduced dependency would
# resolve fine and this check would go green on a machine no user resembles.
if "$BREW" tap | grep -qx "libkrun/krun"; then
  echo "FAIL: the libkrun/krun tap is present on this machine." >&2
  echo "      A fresh user does not have it; with it tapped (and possibly" >&2
  echo "      trusted) the dependency resolution this check exercises is not" >&2
  echo "      the one a fresh user hits." >&2
  exit 1
fi
echo "    ok: the libkrun/krun tap is not present"

# --------------------------------------------------------------------------
# 2. Build the binary the formula will install, and stage it as the archive
#    the formula fetches.
# --------------------------------------------------------------------------
WORK="$(mktemp -d)"
echo "==> Building mgit"
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" \
  -o "$WORK/mgit" "$REPO_ROOT/cmd/mgit/"
tar -czf "$WORK/mgit.tar.gz" -C "$WORK" mgit
SHA="$(shasum -a 256 "$WORK/mgit.tar.gz" | cut -d' ' -f1)"

# --------------------------------------------------------------------------
# 3. Serve brew/mgit.rb from a scratch tap.
#
# A scratch TAP, not a bare formula path: Homebrew's untrusted-tap gate keys
# off the tap a formula was loaded from, so a formula installed by path would
# skip the very mechanism under test. The tap is third-party and untrusted,
# exactly like hyper-swe/tap.
#
# Only the version/url/sha256 lines are rewritten -- the four url/sha256 pairs
# in brew/mgit.rb are documented placeholders that the tap's own automation
# overwrites at every release, so they carry no meaning to preserve. Every
# other line, including the dependency declarations this check exists to
# police, is the repo's formula verbatim.
# --------------------------------------------------------------------------
echo "==> Serving brew/mgit.rb from scratch tap $TAP"
"$BREW" tap-new "$TAP" --no-git >/dev/null
TAP_FORMULA_DIR="$("$BREW" --repository "$TAP")/Formula"
mkdir -p "$TAP_FORMULA_DIR"
sed \
  -e "s|^  version \".*\"|  version \"${VERSION}\"|" \
  -e "s|^      url \".*\"|      url \"file://${WORK}/mgit.tar.gz\"|" \
  -e "s|^      sha256 \".*\"|      sha256 \"${SHA}\"|" \
  "$REPO_ROOT/brew/mgit.rb" > "$TAP_FORMULA_DIR/mgit.rb"

# Guard the rewrite itself: if the sed silently matched nothing (a reformatted
# formula), the install below would fail for a reason that has nothing to do
# with this check, and the failure message would send the next reader hunting
# a dependency bug that is not there.
if ! grep -q "file://${WORK}/mgit.tar.gz" "$TAP_FORMULA_DIR/mgit.rb"; then
  echo "FAIL: could not point the formula at the local archive." >&2
  echo "      brew/mgit.rb's url/sha256 lines were reformatted; update the sed above." >&2
  exit 1
fi

# --------------------------------------------------------------------------
# 4. THE CHECK: a real install, not a dry run.
#
# `brew install` and not `brew install --dry-run`, because the dry run is
# precisely what passed while the bug was live. This resolves dependencies,
# fetches, runs the formula's install block and links the result.
# --------------------------------------------------------------------------
echo "==> brew install $FORMULA"
if ! "$BREW" install "$FORMULA"; then
  echo >&2
  echo "FAIL: \`brew install\` failed on a machine without libkrun." >&2
  echo "      If the error above mentions an untrusted tap or an unavailable" >&2
  echo "      formula, brew/mgit.rb has regained a third-party-tap dependency" >&2
  echo "      (MGIT-75). Core mgit does not need one: it is CGO-free and never" >&2
  echo "      links libkrun. Document the sandbox activation in caveats instead." >&2
  exit 1
fi

echo "==> brew test $FORMULA"
"$BREW" test "$FORMULA"

echo "==> Installed mgit must actually run"
"$("$BREW" --prefix)/bin/mgit" --version

# The claim the caveats make -- "Core mgit (init, commit, worktrees, squash,
# land) is ready to use" -- is the whole justification for dropping the
# dependency, so it is exercised rather than asserted. If core mgit needed
# libkrun after all, this is where it would show.
echo "==> Core mgit must work with no hypervisor present"
CORE="$WORK/corerepo"
mkdir -p "$CORE"
(
  cd "$CORE"
  git init -q .
  git config user.email mgit75@example.com
  git config user.name "MGIT-75 check"
  echo hello > file.txt
  MGIT="$("$BREW" --prefix)/bin/mgit"
  "$MGIT" init
  "$MGIT" commit --task-id MGIT-75 -m "core commit without libkrun"
  "$MGIT" log --oneline
  "$MGIT" verify --task-id MGIT-75
)

echo
echo "PASS: brew install of mgit succeeds, and core mgit works, with libkrun absent."
