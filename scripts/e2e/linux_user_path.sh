#!/usr/bin/env bash
# The documented sandbox user path, on a stock host, against a RELEASE-SHAPED
# install layout: compose a guest base, launch, exec, sync a host edit into
# the running guest, export a guest file, remove. This is what a release user
# does after installing, and what the agent loop lives on.
#
# NOTHING TEST-ONLY IS ALLOWED. mgit's own Linux CI boots firecracker through
# MGIT_GUEST_KERNEL / MGIT_GUEST_ROOTFS, hooks a release user never has, so a
# green there says nothing about the user path. This script refuses to run
# with any of them set.
#
# Each step prints PASS or the exact failure. The LAST line is the verdict,
# `LINUX USER PATH: PASS` or `LINUX USER PATH: FAIL at <step>`, and the exit
# code agrees with it. On failure the daemon's own view (status, doctor) is
# printed, so a red run says why without a rerun.
#
# Usage: linux_user_path.sh <install-dir> [scratch-root]
#   install-dir holds mgit, mgit-sandboxd and guest/ exactly as a release
#   archive lays them out. scratch-root holds the repository and the
#   worktree; it defaults to the temp dir (/tmp on a stock runner).
# Refs: MGIT-229, hyper-swe/mgit#12
set -u

BIN="${1:?usage: linux_user_path.sh <install-dir> [scratch-root]}"
for hook in MGIT_GUEST_KERNEL MGIT_GUEST_ROOTFS MGIT_GUEST_BASE MGIT_GUEST_IMAGE; do
	if [ -n "${!hook:-}" ]; then
		echo "REFUSING: $hook is set — this leg proves the user path WITHOUT test hooks"
		exit 2
	fi
done
export PATH="$BIN:$PATH"
# A scratch root outside /tmp puts the worktree where the guest must make
# its mount point by shadowing a directory the base image ships
# (MGIT-230.7); one CI leg passes one. Refs: MGIT-230.7, MGIT-230.3
ROOT="${2:-${TMPDIR:-/tmp}}"
W="$(mktemp -d "$ROOT/linux-user-path.XXXXXX")" || { echo "cannot make a scratch dir under $ROOT"; exit 2; }
R="$W/repo"
P="$W/work"
TASK=UP-1

fail() {
	echo "  FAIL: $2"
	echo "── the daemon's view:"
	(cd "$R" 2>/dev/null && mgit sandbox status "$TASK" 2>&1 | head -12)
	(cd "$P" 2>/dev/null && mgit doctor 2>&1 | grep -E '^(FAIL|\?)' | head -12)
	(cd "$R" 2>/dev/null && mgit sandbox daemons stop --repo-root "$R" >/dev/null 2>&1)
	echo "(scratch kept for inspection: $W)"
	echo "LINUX USER PATH: FAIL at $1"
	exit 1
}
step() { printf '\n== %s ==\n' "$1"; }
# first reports the head of a verb's output, where its error line is. mgit
# follows an unplaced failure with a generic footer, so the TAIL of a failed
# run is the footer and never the cause (a first run of this leg printed only
# the footer and hid the error).
first() { printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | head -"${2:-3}"; }

step "1 host"
uname -sm
if [ "$(uname -s)" = Linux ]; then
	[ -r /dev/kvm ] && [ -w /dev/kvm ] || fail "host" "/dev/kvm is not readable and writable by $(id -un)"
fi
echo "  PASS"

step "2 the installed layout"
mgit --version || fail "layout" "mgit does not run"
mgit-sandboxd --version || fail "layout" "mgit-sandboxd does not run"
# guest/ beside the binaries (the extracted archive), or ../libexec/guest
# (install.sh, Homebrew) — the two places mgit itself looks. Refs: MGIT-230.1
gdir="$BIN/guest"
[ -d "$gdir" ] || gdir="$BIN/../libexec/guest"
for g in mgit mgit-guest; do
	[ -x "$gdir/$g" ] || fail "layout" "$g is missing from $BIN/guest and $BIN/../libexec/guest"
done
echo "  PASS"

step "3 a repository"
mkdir -p "$R" "$P"
(cd "$R" && git init -q && git -c user.email=up@mgit.local -c user.name=up commit -q --allow-empty -m init &&
	mgit init >/dev/null) || fail "repository" "git init / mgit init failed"
printf 'v1\n' >"$P/f.txt"
# The physical path, because the guest works at the canonical one.
PH="$(cd "$P" && pwd -P)"
# Under /tmp or outside it, comparing physical paths on both sides:
# where /tmp is a symlink (macOS: /tmp -> /private/tmp) a literal "/tmp/*"
# would call a worktree under /tmp "outside". Refs: MGIT-266
where_is() { local root; root="$(cd "$2" && pwd -P)" || return 1; case "$1/" in "$root"/*) echo "under /tmp" ;; *) echo "outside /tmp" ;; esac; }
where="$(where_is "$PH" /tmp)"
echo "  worktree: $PH ($where)"
echo "  PASS"

# fetch-guard: `mgit sandbox base from` pulls an OCI image through the
# product's own registry client (internal/sandboxd/guestbase/pull.go), which
# already bounds a whole pull at 15 minutes -- clause 2, in Go. Wrapping the
# CLI here would guard the wrong layer: a retry outside the client cannot
# clear the half-written blob cache inside it. MGIT-145 carries that work.
# This leg composes exactly as a user does, so it pulls exactly as a user
# does. Refs: MGIT-143, MGIT-145
step "4 compose the release's guest base (mgit sandbox base from)"
out="$(cd "$R" && mgit sandbox base from 2>&1)" || fail "compose" "$(first "$out")"
printf '%s\n' "$out" | grep -E '^(Composing|Registered)' || fail "compose" "no base was registered"
echo "  PASS"

step "5 launch"
out="$(cd "$R" && mgit sandbox launch --task-id "$TASK" --worktree "$P" 2>&1)" ||
	fail "launch" "$(first "$out")"
printf '%s\n' "$out" | head -2
echo "  PASS"

step "6 exec (the first use boots the guest)"
out="$(cd "$P" && timeout 300 mgit run -- sh -c 'echo boot-ok; cat f.txt' 2>&1)"
first "$out" 6
printf '%s\n' "$out" | grep -qx 'boot-ok' || fail "exec" "the guest did not run the command: $(first "$out" 1)"
printf '%s\n' "$out" | grep -qx 'v1' || fail "exec" "the guest does not see the worktree"
# The worktree is mounted at its IDENTICAL host path, and mgit run works
# there, under /tmp or outside it. Refs: MGIT-230.3, MGIT-230.7
gwd="$(cd "$P" && timeout 120 mgit run -- pwd 2>&1)"
[ "$gwd" = "$PH" ] || fail "exec" "the guest works in '$gwd', not the worktree's host path $PH ($where)"
echo "  the guest works in the worktree at its host path ($where)"
echo "  PASS"

step "7 sync a host edit into the running guest"
printf 'v2\n' >"$P/f.txt"
out="$(cd "$R" && mgit sandbox sync --task-id "$TASK" --force 2>&1)" || fail "sync" "$(first "$out")"
printf '%s\n' "$out" | head -2
got="$(cd "$P" && timeout 120 mgit run -- cat f.txt 2>&1)"
[ "$got" = v2 ] || fail "sync" "the guest read '$got' after the sync, not v2"
echo "  PASS"

step "7b the loop's per-round canary: a host delete is gone from the guest right after the sync"
# hyperswe's loop checks this every round, in exactly this shape: two files
# written in the same second with different lengths, a classifying dry run,
# a sync, the guest reads both; then one is deleted on the host, synced, and
# the guest's [ -e ] must say it is gone AT ONCE. On Linux libkrun the guest
# caches a looked-up name for ~5 s (MGIT-90), so this is the sync's settle
# step (MGIT-192) earning its keep, and the delete-bearing sync's time is
# printed, because a loop pays it every round. Refs: MGIT-230.2
ms() { python3 -c 'import time; print(int(time.time() * 1000))'; }
printf 'c\n' >"$P/canary-a.txt"
printf 'canary-b\n' >"$P/canary-b.txt"
out="$(cd "$R" && mgit sandbox sync --task-id "$TASK" --dry-run 2>&1)" || fail "canary" "dry run: $(first "$out")"
out="$(cd "$R" && mgit sandbox sync --task-id "$TASK" 2>&1)" || fail "canary" "sync: $(first "$out")"
got="$(cd "$P" && timeout 120 mgit run -- sh -c 'cat canary-a.txt canary-b.txt' 2>&1)"
[ "$got" = "$(printf 'c\ncanary-b')" ] || fail "canary" "the guest read '$got', not both canary files"
rm "$P/canary-a.txt"
t0="$(ms)"
out="$(cd "$R" && mgit sandbox sync --task-id "$TASK" 2>&1)" || fail "canary" "sync of the delete: $(first "$out")"
t1="$(ms)"
printf '%s\n' "$out" | head -2
seen="$(cd "$P" && timeout 120 mgit run -- sh -c '[ -e canary-a.txt ] && echo present || echo gone' 2>&1)"
[ "$seen" = gone ] || fail "canary" "the guest still sees canary-a.txt right after the sync that deleted it ('$seen')"
echo "  the delete-bearing sync took $((t1 - t0)) ms"
# Whether a cache drop takes effect as the agent's identity: the drop
# command the settle step sends, run through `mgit run`, which runs as the
# daemon's identity. It is NOT a measurement of the settle's own drop: the
# settle's execs do not use this identity (MGIT-270). The line names the
# identity it measured so that it cannot be read as the settle's.
# Refs: MGIT-230.2, MGIT-270
drop="$(cd "$P" && timeout 120 mgit run -- sh -c 'sync; echo 2 > /proc/sys/vm/drop_caches && echo took-effect || echo did-not-take-effect' 2>/dev/null | tail -1)"
[ -n "$drop" ] || drop="cannot tell (the exec did not answer)"
echo "  a cache drop as the agent's identity (mgit run; not the settle step's identity): $drop"
echo "  PASS"

step "7c the loop's exec contract: background survives, /tmp persists, exit codes pass, /proc and dmesg read"
# hyperswe drives long guest commands by starting them with `nohup … &` in
# `mgit run -- /bin/sh -c`, keeping pid/log/rc files under guest /tmp, and
# polling with later execs; it reads /proc/<pid>/stat, /proc/meminfo, nproc
# and dmesg as the exec identity and needs the exit code unchanged. Each
# clause is checked here, as that identity. Refs: MGIT-230.3
gx() { (cd "$P" && timeout 120 mgit run -- /bin/sh -c "$1" 2>&1); }
out="$(gx 'nohup sleep 120 >/tmp/up-bg.log 2>&1 & echo $! >/tmp/up-bg.pid; echo started')"
[ "$(printf '%s\n' "$out" | tail -1)" = started ] || fail "exec contract" "could not start a background command: $(first "$out")"
out="$(gx 'kill -0 "$(cat /tmp/up-bg.pid)" && echo alive || echo dead')"
[ "$(printf '%s\n' "$out" | tail -1)" = alive ] ||
	fail "exec contract" "a nohup'd command did not outlive the exec that started it, or /tmp did not persist: $out"
gx 'kill "$(cat /tmp/up-bg.pid)"' >/dev/null
(cd "$P" && timeout 120 mgit run -- /bin/sh -c 'exit 7' >/dev/null 2>&1)
rc=$?
[ "$rc" -eq 7 ] || fail "exec contract" "a guest exit 7 came back as $rc"
out="$(gx 'head -c 1 /proc/self/stat >/dev/null && echo proc-stat-ok; head -1 /proc/meminfo; nproc; dmesg >/dev/null 2>&1 && echo dmesg-ok || echo dmesg-refused')"
printf '%s\n' "$out" | sed 's/^/  guest: /'
for want in proc-stat-ok MemTotal dmesg-ok; do
	printf '%s\n' "$out" | grep -q "$want" || fail "exec contract" "the exec identity could not read what the loop reads ($want missing): $out"
done
printf '%s\n' "$out" | grep -Eqx '[0-9]+' || fail "exec contract" "nproc printed no CPU count: $out"
# The identity has a NAME in the guest: the guest writes it a passwd entry
# named agent (MGIT-151), and tools that look the user up (whoami, git's
# default identity, Node's os.userInfo()) fail without one.
name="$(gx 'id -un')"
if [ "$name" != agent ]; then
	echo "  the guest's view of this identity:"
	gx 'id; echo "--- /etc/passwd head:"; head -3 /etc/passwd; echo "--- entries for this uid or agent:"; grep -n -e ":$(id -u):" -e "^agent:" /etc/passwd; echo "--- /etc is:"; grep " /etc " /proc/mounts; echo "--- as this identity:"; stat -c "%A %u:%g %n" /etc /etc/passwd /etc/group' | sed 's/^/    /'
	echo "    --- as root (an audited privileged exec):"
	(cd "$R" && timeout 120 mgit sandbox exec --task-id "$TASK" --as-root -- /bin/sh -c 'stat -c "%A %u:%g %n" / /etc /etc/passwd /etc/group /etc/nsswitch.conf; grep -n "^agent:" /etc/passwd' 2>&1) | sed 's/^/    /'
	fail "exec contract" "the exec identity has no name in the guest: id -un said '$name', not agent"
fi
echo "  PASS"

step "8 export a file the guest made"
# The guest path is worktree-relative: export reads the guest's own view of
# the worktree, the airlock through which artifacts leave (besides land).
(cd "$P" && timeout 120 mgit run -- sh -c 'mkdir -p out && echo made-in-guest > out/exported.txt') >/dev/null 2>&1 ||
	fail "export" "the guest could not write out/exported.txt"
out="$(cd "$R" && mgit sandbox export --task-id "$TASK" out/exported.txt "$W/exported.txt" 2>&1)" ||
	fail "export" "$(first "$out")"
[ "$(cat "$W/exported.txt" 2>/dev/null)" = made-in-guest ] || fail "export" "the exported file is missing or wrong"
echo "  PASS"

step "9 remove"
(cd "$R" && mgit sandbox remove "$TASK" --force >/dev/null 2>&1) || fail "remove" "remove failed"
if (cd "$R" && mgit sandbox status "$TASK" >/dev/null 2>&1); then
	fail "remove" "the sandbox is still there after remove"
fi
(cd "$R" && mgit sandbox daemons stop --repo-root "$R" >/dev/null 2>&1)
rm -rf "$W"
echo "  PASS"

echo "LINUX USER PATH: PASS"
