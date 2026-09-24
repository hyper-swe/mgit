#!/usr/bin/env bash
# Install what scripts/release/build-linux-sandboxd.sh needs, inside a fresh
# ubuntu:20.04 container, as root: the libkrun/libkrunfw build toolchain
# (a kernel compile, bindgen's libclang-18 from focal-updates, patchelf, and
# cpio — libkrunfw's aarch64 kernel config sets CONFIG_IKHEADERS=y, whose
# kheaders archive the kernel build makes with cpio; the x86_64 config does
# not, so only the arm64 build needs it), the
# pinned Go toolchain for this architecture, and rustup with the musl target
# krun-init-blob links against. The same recipe the CI libkrun jobs run, in
# one place so the release, the e2e leg and a developer's docker run agree.
#
# Every fetch goes through the one guard (bounded retry, per-attempt timeout,
# precondition restore). The bounds are the CI libkrun jobs' measured ones.
# Refs: MGIT-229, MGIT-87, MGIT-143
#
# Usage: scripts/release/linux-build-prereqs.sh    (as root; sets nothing in
# the caller's environment — it prints the PATH additions on its last line)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
GUARD="$here/../ci/guard-fetch.sh"
GO_VERSION="${MGIT_GO_VERSION:-1.26.6}"

[ "$(id -u)" = 0 ] || { echo "linux-build-prereqs: run as root (inside the build container)" >&2; exit 1; }
case "$(uname -m)" in
x86_64) goarch=amd64 ;;
aarch64) goarch=arm64 ;;
*) echo "linux-build-prereqs: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

apt_restore='rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/partial/*; dpkg --configure -a'
# -q, not -qq: apt then prints each fetch as it happens, so a slow mirror is
# visible while it is slow rather than as a silent bound expiring (MGIT-229).
"$GUARD" -t 300 -l apt-update -c "$apt_restore" -- apt-get update -q
DEBIAN_FRONTEND=noninteractive "$GUARD" -t 600 -l apt-install-libkrun-prereqs -c "$apt_restore" -- \
	apt-get install -y -q --no-install-recommends \
	build-essential flex bison libelf-dev python3-pyelftools bc cpio \
	pkg-config curl git ca-certificates patchelf binutils \
	libclang1-18 libclang-18-dev libclang-common-18-dev libllvm18

if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go$GO_VERSION "; then
	"$GUARD" -t 120 -l go-toolchain-tarball -c 'rm -f /tmp/go.tgz' -- \
		curl -fsSL "https://go.dev/dl/go$GO_VERSION.linux-$goarch.tar.gz" -o /tmp/go.tgz
	rm -rf /usr/local/go
	tar -C /usr/local -xzf /tmp/go.tgz
	rm -f /tmp/go.tgz
fi

if [ ! -x "$HOME/.cargo/bin/rustup" ]; then
	"$GUARD" -t 120 -l rustup-installer -c 'rm -f /tmp/rustup.sh' -- \
		curl -sSf https://sh.rustup.rs -o /tmp/rustup.sh
	"$GUARD" -t 120 -l rustup-toolchain -c 'rm -rf "$HOME/.rustup" "$HOME/.cargo"' -- \
		sh /tmp/rustup.sh -y --default-toolchain stable
fi
"$GUARD" -t 120 -l rustup-musl-target \
	-c none:'rustup unpacks a component into a temp directory and moves it into place only once the download is complete and hash-checked; a failed target add leaves the toolchain exactly as it was' -- \
	"$HOME/.cargo/bin/rustup" target add "$(uname -m)-unknown-linux-musl"

echo "PATH additions: /usr/local/go/bin:$HOME/.cargo/bin"
