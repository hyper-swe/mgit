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
# THE ONE SETUP STEP. The formula now depends on libkrunfw from the
# maintainers' own tap, hyper-swe/tap (MGIT-306). Homebrew trusts the formula
# named on the command line, but refuses that formula's dependency from a tap
# nobody trusted, so the documented install is two commands:
#
#   brew trust hyper-swe/tap
#   brew install hyper-swe/tap/mgit
#
# This check takes exactly that step and no other: it trusts hyper-swe/tap,
# never sets HOMEBREW_NO_REQUIRE_TAP_TRUST and never trusts a third-party tap,
# and proves libkrunfw was absent before the install and linked where the
# sandbox looks for it after it. A dependency on any OTHER tap still fails
# here, on the untrusted-tap gate, which is the MGIT-75 guard.
#
# Until hyper-swe/tap's default branch carries Formula/libkrunfw.rb, the
# dependency cannot resolve from it. MGIT_TAP_BRANCH names a branch of
# hyper-swe/homebrew-tap to resolve hyper-swe/tap from instead, so a change
# to both repositories can be proved before either is merged.
#
# Usage:
#   scripts/brew-install-no-libkrun.sh              # uses `brew` from PATH
#   BREW=/path/to/scratch/brew scripts/brew-install-no-libkrun.sh
#   MGIT_TAP_BRANCH=<branch> scripts/brew-install-no-libkrun.sh
#
# Refs: MGIT-306, MGIT-75, MGIT-69, MGIT-70

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
OWN_TAP="hyper-swe/tap"
TAP_BRANCH="${MGIT_TAP_BRANCH:-}"
case "$TAP_BRANCH" in
"" | [A-Za-z0-9]*) ;;
*) echo "FAIL: MGIT_TAP_BRANCH '$TAP_BRANCH' is not a branch name" >&2; exit 1 ;;
esac
case "$TAP_BRANCH" in
*[!A-Za-z0-9._/-]* | *..*) echo "FAIL: MGIT_TAP_BRANCH '$TAP_BRANCH' is not a branch name" >&2; exit 1 ;;
esac

# Every step is `|| true`: this runs on EXIT, including the failure exits, and
# a cleanup step that fails must not overwrite the status the check produced.
cleanup() {
  "$BREW" uninstall --force "$FORMULA" >/dev/null 2>&1 || true
  "$BREW" uninstall --force "$OWN_TAP/libkrunfw" >/dev/null 2>&1 || true
  "$BREW" untap "$TAP" >/dev/null 2>&1 || true
  if [ -n "${TRUSTED_HERE:-}" ]; then "$BREW" untrust "$OWN_TAP" >/dev/null 2>&1 || true; fi
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
# fetch-guard: `brew tap` with no arguments LISTS the taps already on disk.
# It makes no network request. Refs: MGIT-143
if "$BREW" tap | grep -qx "libkrun/krun"; then
  echo "FAIL: the libkrun/krun tap is present on this machine." >&2
  echo "      A fresh user does not have it; with it tapped (and possibly" >&2
  echo "      trusted) the dependency resolution this check exercises is not" >&2
  echo "      the one a fresh user hits." >&2
  exit 1
fi
echo "    ok: the libkrun/krun tap is not present"

# The formula's one dependency must also have to arrive: libkrunfw absent, and
# hyper-swe/tap not yet trusted, as on a fresh Mac. Refs: MGIT-306
if "$BREW" list --versions libkrunfw >/dev/null 2>&1; then
  echo "FAIL: libkrunfw is already installed on this machine; the install could not" >&2
  echo "      prove that the formula brings it." >&2
  exit 1
fi
if ! TRUSTED_TAPS="$("$BREW" trust --json=v1 --tap)"; then
  echo "FAIL: this Homebrew cannot list its trusted taps (\`brew trust --json=v1 --tap\`)," >&2
  echo "      so whether $OWN_TAP is already trusted is unknown." >&2
  exit 1
fi
if printf '%s\n' "$TRUSTED_TAPS" | grep -q "\"$OWN_TAP"; then
  echo "FAIL: $OWN_TAP is already trusted here; a fresh user has not trusted it yet." >&2
  exit 1
fi
echo "    ok: libkrunfw is not installed and $OWN_TAP is not trusted"

# --------------------------------------------------------------------------
# 2. Build the binary the formula will install, and stage it as the archive
#    the formula fetches.
# --------------------------------------------------------------------------
WORK="$(mktemp -d)"
echo "==> Building mgit"
CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/hyper-swe/mgit/internal/buildinfo.version=${VERSION}" \
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
# fetch-guard: `tap-new --no-git` creates an empty local tap directory. No
# remote is contacted -- that is what --no-git means here. Refs: MGIT-143
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
# 4. The one documented setup step, the tap itself, then (only when
#    MGIT_TAP_BRANCH is set) the tap branch the dependency resolves from.
# --------------------------------------------------------------------------
echo "==> brew trust $OWN_TAP (the documented setup step)"
"$BREW" trust hyper-swe/tap
TRUSTED_HERE=1
# `brew trust` does not tap: without this step the dependency cannot resolve
# at all. The default remote, so the trust recorded above matches the tap.
# -t 300 is provisional: this step has no run history yet (MGIT-143).
echo "==> brew tap $OWN_TAP"
"$REPO_ROOT/scripts/ci/guard-fetch.sh" -t 300 -l brew-tap-own-tap \
  -c "$BREW untap $OWN_TAP >/dev/null 2>&1 || true" -- \
  "$BREW" tap "$OWN_TAP"
if [ -n "$TAP_BRANCH" ]; then
  echo "==> Resolving $OWN_TAP from its branch $TAP_BRANCH"
  OWN_TAP_DIR="$("$BREW" --repository "$OWN_TAP")"
  "$REPO_ROOT/scripts/ci/guard-fetch.sh" -t 300 -l brew-tap-own-tap-branch \
    -c none:'git fetch writes objects before it moves any ref, so a failed fetch leaves the checkout as it was' -- \
    git -C "$OWN_TAP_DIR" fetch --depth 1 origin "$TAP_BRANCH"
  git -C "$OWN_TAP_DIR" checkout -q FETCH_HEAD
  # An auto-update before the install would move the tap back to its default
  # branch.
  export HOMEBREW_NO_AUTO_UPDATE=1
fi

# --------------------------------------------------------------------------
# 5. THE CHECK: a real install, not a dry run.
#
# `brew install` and not `brew install --dry-run`, because the dry run is
# precisely what passed while the bug was live. This resolves dependencies,
# fetches, runs the formula's install block and links the result.
# --------------------------------------------------------------------------
echo "==> brew install $FORMULA"
# The formula's own archive is a file:// URL, but resolving and installing its
# DEPENDENCIES pulls bottles from the same third-party CDN that returned 504
# for half an hour on 2026-08-15. -t 300 is the MGIT-143 rule applied to this
# step's history: worst SUCCESSFUL run 65s across n=111, 4x that up the ladder.
#
# The dependency now includes libkrunfw, which hyper-swe/tap builds from
# source until its bottles are published: a compile of upstream's generated
# kernel.c. That build has no run history yet, so the bound is provisional,
# 1500s, to be re-derived from its history under the same rule.
# Refs: MGIT-143, MGIT-119, MGIT-306
if ! "$REPO_ROOT/scripts/ci/guard-fetch.sh" -t 1500 -l brew-install-mgit-formula \
  -c none:'brew verifies the checksum of everything it reuses and keeps a partial download as .incomplete, which it resumes rather than re-extracting' -- \
  "$BREW" install "$FORMULA"; then
  echo >&2
  echo "FAIL: \`brew install\` failed on a machine without libkrun." >&2
  echo "      If the error above mentions an untrusted tap, brew/mgit.rb depends on a" >&2
  echo "      tap other than $OWN_TAP (MGIT-75); only $OWN_TAP is covered by the" >&2
  echo "      documented trust step. If it says $OWN_TAP has no libkrunfw formula, the" >&2
  echo "      tap's default branch does not carry it yet: set MGIT_TAP_BRANCH (MGIT-306)." >&2
  exit 1
fi

echo "==> libkrunfw must arrive with mgit, linked where the sandbox looks"
LIBKRUNFW="$("$BREW" --prefix)/lib/libkrunfw.5.dylib"
if [ ! -e "$LIBKRUNFW" ]; then
  echo "FAIL: $LIBKRUNFW is missing after the install; the sandbox searches" >&2
  echo "      $("$BREW" --prefix)/lib for libkrunfw by its file name." >&2
  exit 1
fi
if find "$("$BREW" --prefix "$FORMULA")/" -name 'libkrunfw*' | grep -q .; then
  echo "FAIL: the mgit keg carries libkrunfw; it must come only from $OWN_TAP/libkrunfw." >&2
  exit 1
fi
echo "    ok: $LIBKRUNFW is linked, and the mgit keg carries no libkrunfw"

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
  MGIT="$("$BREW" --prefix)/bin/mgit"
  "$MGIT" init
  # Written AFTER init: a file that is there at init is the base (FEAT-6.116),
  # so committing it would be an empty commit and be refused.
  echo hello > file.txt
  # Stage before committing: MGIT-77 made `mgit commit` REFUSE an empty commit
  # rather than report success for work it never recorded, which turned this
  # smoke test red — it had been relying on the old implicit behavior. Staging
  # is also what the documented loop tells a first-hour user to do.
  # Refs: MGIT-77, MGIT-78
  "$MGIT" add file.txt
  "$MGIT" commit --task-id MGIT-75 -m "core commit without libkrun"
  "$MGIT" log --oneline
  "$MGIT" verify --task-id MGIT-75
)

echo
echo "PASS: after \`brew trust $OWN_TAP\`, brew install of mgit succeeds with libkrun absent,"
echo "      brings libkrunfw from $OWN_TAP, and core mgit works."
