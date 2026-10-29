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
#   MGIT_SANDBOX        1 to set up the microVM sandbox's one external library,
#                       libkrunfw, on Apple silicon macOS (see below). Off by
#                       default: nothing beyond mgit's own archive is fetched.
#   MGIT_LIBKRUNFW_ACCEPT
#                       yes or no: the answer to the libkrunfw license question,
#                       for a run with no terminal to ask on.
#   MGIT_LIBKRUNFW_BASE where libkrunfw's tarball and its checksums.txt are
#                       fetched from (a URL, or file://… for an offline copy).
#
# Layout, matching what mgit itself looks for (cmd/mgit/sandbox_base.go):
#   $PREFIX/bin/mgit                    the CLI
#   $PREFIX/bin/mgit-sandboxd           the sandbox daemon, where shipped
#   $PREFIX/libexec/guest/{mgit,mgit-guest}
#       the LINUX guest pair `mgit sandbox base from <image>` injects. It goes
#       in libexec, never bin: everything in bin lands on PATH, and mgit-guest
#       is guest-only — it refuses to run on a host. Refs: MGIT-65
#   $PREFIX/lib/mgit/{libkrun.so.1,libkrunfw.so.5} (Linux), libkrun.1.dylib (macOS)
#       the hypervisor libraries the daemon is linked against, where its run
#       path looks ($ORIGIN/../lib/mgit, @executable_path/../lib/mgit).
#       Refs: MGIT-230.1, MGIT-259
#   $PREFIX/share/mgit/THIRD_PARTY/
#       their license texts and the note naming their published source.
#   $PREFIX/lib/mgit/libkrunfw.5.dylib, $PREFIX/share/mgit/THIRD_PARTY/libkrunfw/
#       macOS only, and only with MGIT_SANDBOX=1: libkrunfw, a SEPARATE download
#       that is never part of mgit's archive, with its license texts, notice and
#       source offer. The daemon looks for it beside libkrun. Refs: MGIT-306
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

# listed_sha256 <checksums.txt> <file name>: the digest the list names for it.
listed_sha256() {
	sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]][[:space:]]*$2\$/\1/p" "$1" | head -1
}
# sha256_of <file>: its digest, by whichever tool this machine has.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "need sha256sum or shasum to verify the download"
	fi
}

want="$(listed_sha256 "$tmp/checksums.txt" "$archive")"
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
got="$(sha256_of "$tmp/$archive")" || exit 1
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
# The Linux and macOS archives bundle the daemon's hypervisor libraries in
# lib/. They go where the daemon's run path looks for them from $PREFIX/bin —
# $PREFIX/lib/mgit — never onto PATH, and their license texts go with them.
# Refs: MGIT-230.1, MGIT-229, MGIT-259
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
# On Linux, and on macOS when the archive carries lib/, the archive carries
# everything the daemon links, so a daemon that does not load here is an
# incomplete install, said now rather than at the first sandbox.
# Refs: MGIT-230.1, MGIT-259
if { [ "$os_name" = linux ] || [ -d "$tmp/lib" ]; } && [ -f "$bindir/mgit-sandboxd" ]; then
	if ! sout="$("$bindir/mgit-sandboxd" --version 2>&1)"; then
		die "the installed sandbox daemon does not load: $sout
  Its libraries belong in $prefix/lib/mgit. Core mgit is installed and works;
  the sandbox will not until the daemon loads."
	fi
	say "$sout"
fi

# --- libkrunfw: a separate, opt-in download (Apple silicon macOS) -----------
# The macOS sandbox needs libkrunfw, the library that carries the guest Linux
# kernel. It is distributed under its own licenses (LGPL-2.1-only for the
# library, GPL-2.0-only for the kernel and its patches) and ships as a
# separate download, not in mgit's macOS archive. This step fetches it, and
# only when the user enables the sandbox (MGIT_SANDBOX=1) and says yes to a
# question that names
# the licenses and the source. It is verified against its own checksums.txt
# and installed beside libkrun, in $PREFIX/lib/mgit, where the daemon looks
# for it, with its license texts, notice and source offer. It never writes
# outside $PREFIX. The Linux archive carries its own libkrunfw. Refs: MGIT-306
#
# The tarball holds, at its top level: libkrunfw.5.dylib, LICENSE-GPL-2.0-only,
# LICENSE-LGPL-2.1-only, NOTICE.txt and SOURCE-OFFER.txt.
LIBKRUNFW_VERSION="5.5.0"
LIBKRUNFW_KERNEL="linux-6.12.91"
LIBKRUNFW_SOURCE="https://github.com/libkrun/libkrunfw/releases/tag/v$LIBKRUNFW_VERSION"
LIBKRUNFW_KERNEL_SOURCE="https://cdn.kernel.org/pub/linux/kernel/v6.x/$LIBKRUNFW_KERNEL.tar.xz"
# The tarball and its checksums.txt are published in the same mgit release as
# the source assets, so the default location is this release.
LIBKRUNFW_DEFAULT_BASE="$base_url"
LIBKRUNFW_FILES="libkrunfw.5.dylib LICENSE-GPL-2.0-only LICENSE-LGPL-2.1-only NOTICE.txt SOURCE-OFFER.txt"

libkrunfw_notice() {
	say ""
	say "==> libkrunfw $LIBKRUNFW_VERSION, the guest kernel library the macOS sandbox needs"
	say "    libkrunfw is a separate library from https://github.com/libkrun/libkrunfw,"
	say "    distributed separately from mgit. It bundles a Linux kernel ($LIBKRUNFW_KERNEL)."
	say "    Licenses: LGPL-2.1-only AND GPL-2.0-only (LGPL-2.1-only for the library code,"
	say "    GPL-2.0-only for the bundled kernel and the patches applied to it)."
	say "    Source:"
	say "      $LIBKRUNFW_SOURCE"
	say "      $LIBKRUNFW_KERNEL_SOURCE"
	say "    Every mgit release also hosts the corresponding source, under its checksums.txt:"
	say "    the kernel tarball and libkrunfw's source, patches and configuration; and, for"
	say "    reference, the prebuilt kernel image (libkrunfw-v$LIBKRUNFW_VERSION-prebuilt-aarch64.tgz) the library is built from."
	say "    If any link stops working, write to swe@hyperswe.com for the complete source,"
	say "    at no charge beyond the cost of physically providing it."
	say "    The download carries both license texts and the full source offer."
}

# libkrunfw_accepted: 0 when the user said yes, 1 when they said no or could
# not be asked. MGIT_LIBKRUNFW_ACCEPT answers without a prompt; otherwise the
# question goes to the terminal, because stdin is this script under `curl | sh`.
libkrunfw_accepted() {
	answer="${MGIT_LIBKRUNFW_ACCEPT:-}"
	if [ -z "$answer" ]; then
		if ! { : </dev/tty; } 2>/dev/null; then
			say "    No terminal to ask on. To accept, run again with MGIT_LIBKRUNFW_ACCEPT=yes."
			return 1
		fi
		printf 'Download libkrunfw %s under these licenses? [y/N] ' "$LIBKRUNFW_VERSION" >/dev/tty
		read -r answer </dev/tty || answer=""
	fi
	case "$answer" in
	y | Y | yes | YES | 1) return 0 ;;
	n | N | no | NO | 0 | "") return 1 ;;
	*) die "MGIT_LIBKRUNFW_ACCEPT must be yes or no, not '$answer'. Core mgit is installed." ;;
	esac
}

install_libkrunfw() {
	base="${MGIT_LIBKRUNFW_BASE-$LIBKRUNFW_DEFAULT_BASE}"
	[ -n "$base" ] || die "this installer has no download location for libkrunfw yet.
  Set MGIT_LIBKRUNFW_BASE to a location that holds $1 and its checksums.txt,
  or install libkrunfw with Homebrew:
    brew trust hyper-swe/tap
    brew install hyper-swe/tap/libkrunfw
  Core mgit is installed and works; the sandbox needs libkrunfw."
	kdir="$tmp/libkrunfw"
	mkdir -p "$kdir/x"
	say "==> downloading libkrunfw $LIBKRUNFW_VERSION ($1)"
	fetch "$base/$1" "$kdir/$1" || die "no $1 at $base. Core mgit is installed."
	fetch "$base/checksums.txt" "$kdir/checksums.txt" || die "no checksums.txt at $base. Core mgit is installed."
	kwant="$(listed_sha256 "$kdir/checksums.txt" "$1")"
	[ -n "$kwant" ] || die "$1 is not listed in $base/checksums.txt. Core mgit is installed."
	kgot="$(sha256_of "$kdir/$1")" || exit 1
	[ "$kwant" = "$kgot" ] || die "checksum mismatch for $1
  expected $kwant
  got      $kgot
  Refusing to install libkrunfw. Core mgit is installed."
	say "    ok: sha256 verified"
	tar -xzf "$kdir/$1" -C "$kdir/x"
	# The library never lands without its license texts and source offer.
	for f in $LIBKRUNFW_FILES; do
		[ -f "$kdir/x/$f" ] && [ ! -L "$kdir/x/$f" ] || die "$1 does not carry $f; refusing to install libkrunfw. Core mgit is installed."
	done
	kdoc="$prefix/share/mgit/THIRD_PARTY/libkrunfw"
	mkdir -p "$kdoc" "$prefix/lib/mgit" || die "cannot create $kdoc or $prefix/lib/mgit"
	for f in $LIBKRUNFW_FILES; do
		[ "$f" = libkrunfw.5.dylib ] || install -m 0644 "$kdir/x/$f" "$kdoc/$f"
	done
	install -m 0644 "$kdir/x/libkrunfw.5.dylib" "$prefix/lib/mgit/libkrunfw.5.dylib"
	say "    installed $prefix/lib/mgit/libkrunfw.5.dylib (beside libkrun)"
	say "    installed $kdoc/ (its license texts, notice and source offer)"
}

sandbox="${MGIT_SANDBOX:-0}"
if [ "$os_name/$arch_name" = darwin/arm64 ] && [ -f "$bindir/mgit-sandboxd" ]; then
	if [ "$sandbox" = 1 ]; then
		libkrunfw_notice
		if libkrunfw_accepted; then
			install_libkrunfw "libkrunfw_${LIBKRUNFW_VERSION}_darwin_arm64.tar.gz"
		else
			say "    libkrunfw was not downloaded. The sandbox needs it; core mgit does not."
		fi
	else
		say ""
		say "The microVM sandbox also needs libkrunfw, a separate library under its own"
		say "licenses. To add it, run this installer again with MGIT_SANDBOX=1, or use"
		say "Homebrew: brew trust hyper-swe/tap && brew install hyper-swe/tap/libkrunfw"
	fi
elif [ "$sandbox" = 1 ] && [ "$os_name" = linux ]; then
	say "    the Linux archive carries libkrunfw; nothing more to fetch"
elif [ "$sandbox" = 1 ]; then
	say "    the sandbox runs on Linux and Apple silicon macOS only; libkrunfw is not fetched"
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
