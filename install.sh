#!/bin/sh
# mgit installer.
#
#   curl -fsSL https://raw.githubusercontent.com/hyper-swe/mgit/main/install.sh | sh
#
# WHY THIS EXISTS, beyond convenience. On macOS, a binary downloaded by a
# BROWSER carries the com.apple.quarantine attribute, and Gatekeeper SIGKILLs a
# quarantined ad-hoc-signed binary on Apple Silicon — no dialog, just
# "zsh: killed" (MGIT-64). The attribute is written by the downloading app on
# the user's machine, so nothing we do at build time can remove it; only
# notarization (a paid Apple Developer certificate) fixes the browser path.
#
# curl is not a quarantine-aware app. Downloading through this script means the
# binaries are never quarantined and simply run. That is the whole reason to
# prefer it over "grab the tarball from the releases page".
#
# Environment:
#   MGIT_VERSION        tag to install (default: the latest release)
#   MGIT_PREFIX         install prefix (default: /usr/local if writable, else ~/.local)
#   MGIT_DOWNLOAD_BASE  where the release's archive and checksums.txt are fetched
#                       from (default: the GitHub release for MGIT_VERSION) — a
#                       mirror or an air-gapped copy (file://…). The checksum is
#                       verified against what was fetched either way.
#
# Layout, matching what mgit itself looks for (cmd/mgit/sandbox_base.go):
#   $PREFIX/bin/mgit                    the CLI
#   $PREFIX/bin/mgit-sandboxd           the sandbox daemon, where shipped
#   $PREFIX/libexec/guest/{mgit,mgit-guest}
#       the LINUX guest pair `mgit sandbox base from <image>` injects. It goes
#       in libexec, never bin: everything in bin lands on PATH, and mgit-guest
#       is guest-only — it refuses to run on a host. Refs: MGIT-65
#   $PREFIX/lib/mgit/{libkrun.so.1,libkrunfw.so.5}
#       Linux only: the hypervisor libraries the daemon is linked against,
#       where its run path looks ($ORIGIN/../lib/mgit). Refs: MGIT-230.1
#   $PREFIX/share/mgit/THIRD_PARTY/
#       their license texts and the note naming their published source.
set -eu

REPO="hyper-swe/mgit"
say() { printf '%s\n' "$*"; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# --- what are we installing onto -------------------------------------------
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
Darwin) os_name="darwin" ;;
Linux) os_name="linux" ;;
*)
	die "unsupported OS: $os
  Windows: download the .zip from https://github.com/$REPO/releases and add it to PATH.
  (Windows runs core mgit; the microVM sandbox is Linux and macOS only.)"
	;;
esac
case "$arch" in
x86_64 | amd64) arch_name="amd64" ;;
arm64 | aarch64) arch_name="arm64" ;;
*) die "unsupported architecture: $arch" ;;
esac

# --- fetch helper: curl or wget, whichever exists --------------------------
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
	fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
	fetch_stdout() { wget -qO- "$1"; }
else
	die "need curl or wget"
fi

# --- which version ---------------------------------------------------------
version="${MGIT_VERSION:-}"
if [ -z "$version" ]; then
	say "==> resolving the latest release"
	# Parse tag_name without requiring jq — this script must run on a machine
	# with nothing installed, which is the entire point of it.
	version="$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
	[ -n "$version" ] || die "could not resolve the latest release; set MGIT_VERSION=vX.Y.Z"
fi
bare="${version#v}"
say "==> mgit $version ($os_name/$arch_name)"

# --- download and VERIFY ---------------------------------------------------
# A checksum check is not ceremony here: this script pipes a downloaded binary
# straight onto your PATH, so it verifies against the checksums.txt published
# with the release before anything is installed.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM
archive="mgit_${bare}_${os_name}_${arch_name}.tar.gz"
base_url="${MGIT_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download/$version}"

say "==> downloading $archive"
fetch "$base_url/$archive" "$tmp/$archive" || die "no archive $archive in release $version"
fetch "$base_url/checksums.txt" "$tmp/checksums.txt" || die "release $version publishes no checksums.txt"

want="$(sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]][[:space:]]*$archive\$/\1/p" "$tmp/checksums.txt" | head -1)"
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	got="$(sha256sum "$tmp/$archive" | cut -d' ' -f1)"
elif command -v shasum >/dev/null 2>&1; then
	got="$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)"
else
	die "need sha256sum or shasum to verify the download"
fi
[ "$want" = "$got" ] || die "checksum mismatch for $archive
  expected $want
  got      $got
  Refusing to install. Report this — it should never happen."
say "    ok: sha256 verified"

# --- install ---------------------------------------------------------------
prefix="${MGIT_PREFIX:-}"
if [ -z "$prefix" ]; then
	# Prefer a system prefix when this user can already write it; never sudo on
	# the user's behalf. Otherwise the per-user prefix, which needs no
	# privileges at all.
	if [ -w /usr/local/bin ] 2>/dev/null; then prefix="/usr/local"; else prefix="$HOME/.local"; fi
fi
bindir="$prefix/bin"
guestdir="$prefix/libexec/guest"
mkdir -p "$bindir" "$guestdir" 2>/dev/null ||
	die "cannot create $bindir — choose another prefix with MGIT_PREFIX=\$HOME/.local"

tar -xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/mgit" ] || die "archive did not contain mgit"
install -m 0755 "$tmp/mgit" "$bindir/mgit"
say "    installed $bindir/mgit"
# Present on Linux and macOS/arm64 only; the other archives ship no sandbox
# backend, and that is expected rather than an error.
if [ -f "$tmp/mgit-sandboxd" ]; then
	install -m 0755 "$tmp/mgit-sandboxd" "$bindir/mgit-sandboxd"
	say "    installed $bindir/mgit-sandboxd"
fi
if [ -d "$tmp/guest" ]; then
	for g in "$tmp"/guest/*; do
		[ -f "$g" ] || continue
		install -m 0755 "$g" "$guestdir/$(basename "$g")"
	done
	say "    installed $guestdir/ (guest pair for 'mgit sandbox base from')"
fi
# The Linux archive bundles the daemon's hypervisor libraries (libkrun and
# libkrunfw) in lib/. They go where the daemon's run path looks for them from
# $PREFIX/bin — $PREFIX/lib/mgit — never onto PATH, and their license texts
# go with them. Refs: MGIT-230.1, MGIT-229
if [ -d "$tmp/lib" ]; then
	libdir="$prefix/lib/mgit"
	mkdir -p "$libdir" || die "cannot create $libdir"
	for l in "$tmp"/lib/*; do
		[ -f "$l" ] || continue
		install -m 0644 "$l" "$libdir/$(basename "$l")"
	done
	say "    installed $libdir/ (the sandbox daemon's hypervisor libraries)"
fi
if [ -d "$tmp/THIRD_PARTY" ]; then
	docdir="$prefix/share/mgit/THIRD_PARTY"
	mkdir -p "$docdir" || die "cannot create $docdir"
	for d in "$tmp"/THIRD_PARTY/*; do
		[ -f "$d" ] || continue
		install -m 0644 "$d" "$docdir/$(basename "$d")"
	done
	say "    installed $docdir/ (license texts and source notice)"
fi

# --- prove it runs ---------------------------------------------------------
# The installed binary, not the one in the temp dir: this is the thing that
# would have been killed had it arrived quarantined, so run it and say so.
if ! out="$("$bindir/mgit" --version 2>&1)"; then
	die "installed mgit does not run: $out"
fi
say ""
say "$out"
# On Linux the archive carries everything the daemon links, so a daemon that
# does not load here is an incomplete install, said now rather than at the
# first sandbox. (On macOS the daemon links a Homebrew libkrun the user
# installs separately, so it is not asked here.) Refs: MGIT-230.1
if [ "$os_name" = linux ] && [ -f "$bindir/mgit-sandboxd" ]; then
	if ! sout="$("$bindir/mgit-sandboxd" --version 2>&1)"; then
		die "the installed sandbox daemon does not load: $sout
  Its libraries belong in $prefix/lib/mgit. Core mgit is installed and works;
  the sandbox will not until the daemon loads."
	fi
	say "$sout"
fi

case ":$PATH:" in
*":$bindir:"*) ;;
*)
	say ""
	say "NOTE: $bindir is not on your PATH. Add it:"
	say "  echo 'export PATH=\"$bindir:\$PATH\"' >> ~/.zshrc   # or ~/.bashrc"
	;;
esac

say ""
say "Next: mgit init && mgit work ../wt --task-id MY-1"
say "Sandbox setup (optional): https://github.com/$REPO/blob/main/docs/INSTALL-SANDBOX.md"
