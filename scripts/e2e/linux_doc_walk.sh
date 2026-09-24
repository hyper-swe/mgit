#!/usr/bin/env bash
# A MEASUREMENT, not a gate: walk docs/INSTALL-SANDBOX.md's Linux path with a
# PUBLISHED mgit release on a stock host, and record every step's command,
# exit code and full output. Unlike linux_user_path.sh it does NOT stop at the
# first failure: the point is to see every step a user meets, including the
# ones after the first refusal (sync, export, policy, doctor, recreate).
#
# Nothing test-only: no MGIT_GUEST_* hook, no source checkout on PATH. The
# release is installed by install.sh, as the README tells a user to.
#
# Usage: linux_doc_walk.sh <version, e.g. v0.6.8>
# Refs: MGIT-229, hyper-swe/mgit#12
set -u

VERSION="${1:?usage: linux_doc_walk.sh <vX.Y.Z>}"
for hook in MGIT_GUEST_KERNEL MGIT_GUEST_ROOTFS MGIT_GUEST_BASE MGIT_GUEST_IMAGE MGIT_GUEST_RELEASE_BASE; do
	if [ -n "${!hook:-}" ]; then
		echo "REFUSING: $hook is set; this walk measures the user path without test hooks"
		exit 2
	fi
done

W="$(mktemp -d "${TMPDIR:-/tmp}/linux-doc-walk.XXXXXX")"
PREFIX="$W/prefix"
R="$W/repo"
P="$W/wt"
TASK=WALK-1
STEP=0
declare -a SUMMARY=()

section() { printf '\n######## %s ########\n' "$*"; }

# run <label> <dir> <cmd...>: print the command, its full output and its exit
# code, and remember one summary line. Never exits.
run() {
	local label="$1" dir="$2"
	shift 2
	STEP=$((STEP + 1))
	printf '\n== [%02d] %s ==\n$ (cd %s) %s\n' "$STEP" "$label" "$dir" "$*"
	local out rc
	out="$(cd "$dir" && timeout 600 "$@" 2>&1)"
	rc=$?
	printf '%s\n[exit %d]\n' "$out" "$rc"
	SUMMARY+=("$(printf '[%02d] exit=%-3d %s' "$STEP" "$rc" "$label")")
	return 0
}

daemon_log() {
	section "daemon log(s) — $1"
	find "$W" "${XDG_RUNTIME_DIR:-/nonexistent}" "$HOME/.cache/mgit" -name daemon.log 2>/dev/null |
		while read -r f; do
			echo "--- $f ($(wc -c <"$f") bytes, last 60 lines)"
			tail -60 "$f"
		done
}

section "host"
uname -a
cat /etc/os-release 2>/dev/null | grep -E '^(PRETTY_NAME|VERSION_ID)='
ldd --version 2>&1 | head -1
id
ls -l /dev/kvm 2>&1
nproc
free -g | head -2

section "install ($VERSION) through install.sh, as the README says"
mkdir -p "$PREFIX" "$R" "$P"
run "install.sh MGIT_VERSION=$VERSION" "$W" env MGIT_VERSION="$VERSION" MGIT_PREFIX="$PREFIX" \
	sh -c 'curl -fsSL "https://raw.githubusercontent.com/hyper-swe/mgit/main/install.sh" | sh'
find "$PREFIX" -type f -exec ls -l {} \;
export PATH="$PREFIX/bin:$PATH"
run "mgit --version" "$W" mgit --version
run "mgit-sandboxd --version" "$W" mgit-sandboxd --version
run "file mgit-sandboxd" "$W" file "$PREFIX/bin/mgit-sandboxd"
run "ldd mgit-sandboxd" "$W" ldd "$PREFIX/bin/mgit-sandboxd"

section "the documented Linux prerequisite: firecracker on PATH"
run "firecracker on PATH before any step (docs name no install command)" "$W" sh -c 'command -v firecracker || echo "firecracker: not on PATH"'
# The docs say only "the firecracker binary on PATH"; a user fetches upstream's
# release. The version is the one mgit pins for its own CI.
FC=v1.13.2
run "fetch firecracker $FC from upstream (not an mgit-documented step)" "$W" sh -c \
	"curl -fsSL https://github.com/firecracker-microvm/firecracker/releases/download/$FC/firecracker-$FC-x86_64.tgz | tar -xz && install -m 0755 release-$FC-x86_64/firecracker-$FC-x86_64 '$PREFIX/bin/firecracker'"
run "firecracker --version" "$W" firecracker --version

section "a repository"
run "git init + mgit init" "$R" sh -c 'git init -q && git -c user.email=w@example.invalid -c user.name=w commit -q --allow-empty -m init && mgit init'
printf 'v1\n' >"$P/f.txt"
run "mgit doctor (fresh repo, nothing provisioned)" "$R" mgit doctor

section "provision the guest base, as the docs say"
run "mgit sandbox image init" "$R" mgit sandbox image init
run "mgit sandbox image install (no --from: the documented firecracker path)" "$R" mgit sandbox image install
run "mgit sandbox base from (no reference: the release's own base)" "$R" mgit sandbox base from
run "mgit sandbox base resolve" "$R" mgit sandbox base resolve
run "mgit doctor (after base from)" "$R" mgit doctor

section "launch, first use"
run "mgit sandbox launch (network none, the default)" "$R" mgit sandbox launch --task-id "$TASK" --worktree "$P"
run "mgit sandbox status (before first use)" "$R" mgit sandbox status "$TASK"
run "mgit run -- echo boot-ok (first use boots the guest)" "$P" mgit run -- sh -c 'echo boot-ok; cat f.txt'
run "mgit sandbox exec" "$R" mgit sandbox exec --task-id "$TASK" -- cat f.txt
run "mgit sandbox status (after first use)" "$R" mgit sandbox status "$TASK"
run "mgit sandbox list" "$R" mgit sandbox list
daemon_log "after the first exec"

section "the loop verbs: sync, export, policy"
printf 'v2\n' >"$P/f.txt"
run "mgit sandbox sync --dry-run" "$R" mgit sandbox sync --task-id "$TASK" --dry-run
run "mgit sandbox sync" "$R" mgit sandbox sync --task-id "$TASK"
run "mgit sandbox export" "$R" mgit sandbox export --task-id "$TASK" f.txt "$W/exported.txt"
run "mgit sandbox policy set --allow registry.npmjs.org:443" "$R" mgit sandbox policy set --task-id "$TASK" --allow registry.npmjs.org:443
run "mgit sandbox policy show" "$R" mgit sandbox policy show --task-id "$TASK"
run "mgit sandbox policy revoke" "$R" mgit sandbox policy revoke --task-id "$TASK"

section "remove and recreate"
run "mgit sandbox remove" "$R" mgit sandbox remove "$TASK" --force
run "mgit sandbox status (after remove)" "$R" mgit sandbox status "$TASK"
run "mgit sandbox launch again (recreate, network allowlist, as the chain does)" "$R" mgit sandbox launch --task-id "$TASK" --worktree "$P" --network allowlist --allow registry.npmjs.org:443
run "mgit run after recreate" "$P" mgit run -- echo recreated
run "mgit sandbox remove (recreated)" "$R" mgit sandbox remove "$TASK" --force

section "mgit work --sandbox (the one-command agent start)"
run "mgit work --sandbox" "$R" mgit work "$W/agent-wt" --task-id WALK-2 --agent-id walker --sandbox
run "mgit run inside the work tree" "$W/agent-wt" mgit run -- echo from-work
run "mgit sandbox remove WALK-2" "$R" mgit sandbox remove WALK-2 --force

section "doctor, final"
run "mgit doctor" "$R" mgit doctor
run "mgit doctor --json" "$R" mgit doctor --json
daemon_log "final"
run "mgit sandbox daemons stop" "$R" mgit sandbox daemons stop --repo-root "$R"

section "the same launch as root, in its own repo (firecracker egress wiring needs host privileges)"
RR="$W/rootrepo"
mkdir -p "$RR"
run "root: git init + mgit init + base from" "$RR" sudo env "PATH=$PATH" sh -c 'git init -q && git -c user.email=w@example.invalid -c user.name=w commit -q --allow-empty -m init && mgit init >/dev/null && mgit sandbox image init >/dev/null && mgit sandbox base from 2>&1 | tail -3'
run "root: launch allowlist" "$RR" sudo env "PATH=$PATH" mgit sandbox launch --task-id WALK-3 --worktree "$P" --network allowlist --allow registry.npmjs.org:443
run "root: first exec" "$RR" sudo env "PATH=$PATH" mgit sandbox exec --task-id WALK-3 -- echo root-boot
run "root: remove + stop" "$RR" sudo env "PATH=$PATH" sh -c "mgit sandbox remove WALK-3 --force; mgit sandbox daemons stop --repo-root '$RR'"

section "SUMMARY (one line per step; exit code is the command's own)"
printf '%s\n' "${SUMMARY[@]}"
echo "LINUX DOC WALK: DONE ($VERSION, ${#SUMMARY[@]} steps)"
