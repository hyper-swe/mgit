#!/usr/bin/env bash
# REST API + serve/CLI lock-coexistence e2e (MGIT-53).
#
# Proves two things against a REAL `mgit serve --api-only` process:
#   A) The documented REST scope (docs/MCP-PARITY.md, MGIT-52) actually works:
#      health, commits create/get/list, task commits, branches create/list,
#      squash, rollback, verify — asserting on real JSON fields, plus a
#      structured 400 on a malformed body.
#   B) Serve/CLI lock coexistence (MGIT-46): while serve runs, CLI commands on
#      the same repo (status/log/commit/worktree add+remove) succeed PROMPTLY —
#      no 30s stall, no "another mgit process is running".
# Also asserts the loopback-only posture (NFR-5.11): serve announces 127.0.0.1
# and, best-effort, is unreachable on a non-loopback interface.
#
# Usage: rest_posture.sh [bindir]
#   bindir (optional): a directory to prepend to PATH containing mgit.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "${1:-}" != "" ]; then export PATH="$1:$PATH"; fi
require_mgit

# --- portable bounded run -----------------------------------------------------
# run_bounded <secs> <what> -- <cmd...>
# Runs cmd with a hard wall-clock bound; prints its combined output and fails
# the e2e if it does not finish in time. GNU coreutils `timeout` is absent on
# stock macOS, so fall back to a perl alarm(2) wrapper (perl ships on both
# macOS and ubuntu CI runners).
run_bounded() {
	local secs="$1" what="$2"
	shift 2
	[ "$1" = "--" ] && shift
	local rc=0 out
	if command -v timeout >/dev/null 2>&1; then
		out="$(timeout "$secs" "$@" 2>&1)" || rc=$?
	else
		out="$(perl -e 'alarm shift @ARGV; exec @ARGV or die "exec: $!"' "$secs" "$@" 2>&1)" || rc=$?
	fi
	# 124 = GNU timeout expiry; 142 = 128+SIGALRM from the perl fallback.
	if [ "$rc" -eq 124 ] || [ "$rc" -eq 142 ]; then
		_e2e_fail "$what — did not finish within ${secs}s (lock starvation?): $*"
	fi
	if [ "$rc" -ne 0 ]; then
		_e2e_fail "$what — command failed (rc=$rc): $*: $out"
	fi
	printf '%s' "$out"
}

# --- scratch project repo -----------------------------------------------------
work="$(mktemp -d)"
server_pid=""
cleanup() {
	if [ -n "$server_pid" ]; then
		kill "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
cd "$work"

echo "== project git + mgit init =="
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m "project init"
mgit init >/dev/null
assert_file ".mgit/index.db" "mgit init created the .mgit store"

# --- start a REAL mgit serve process -------------------------------------------
pick_port() {
	if command -v python3 >/dev/null 2>&1; then
		python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
	else
		echo $(( (RANDOM % 20000) + 30000 ))
	fi
}

echo "== start mgit serve --api-only (real background process) =="
port=""
for attempt in 1 2 3; do
	port="$(pick_port)"
	mgit serve --api-only --port "$port" >"$work/serve.log" 2>&1 &
	server_pid=$!
	up=""
	for _ in $(seq 1 50); do # bounded ~10s
		if curl -sf "http://127.0.0.1:$port/health" >/dev/null 2>&1; then up=1; break; fi
		kill -0 "$server_pid" 2>/dev/null || break # died (port collision?) — retry
		sleep 0.2
	done
	[ -n "$up" ] && break
	kill "$server_pid" 2>/dev/null || true
	wait "$server_pid" 2>/dev/null || true
	server_pid=""
	echo "  (serve did not come up on port $port, attempt $attempt)"
done
[ -n "$server_pid" ] || _e2e_fail "mgit serve never became healthy: $(cat "$work/serve.log")"
base="http://127.0.0.1:$port"
pass "serve is up on $base (pid $server_pid)"

# --- A) documented REST surface (docs/MCP-PARITY.md REST scope) -----------------
echo "== REST: GET /health =="
out="$(curl -s "$base/health")"
assert_contains "$out" '"status":"ok"' "health returns status ok"
assert_contains "$out" '"timestamp"' "health carries a timestamp"

echo "== REST: POST /api/v1/commits (commits the CLI-staged change) =="
printf 'hello rest\n' > rest.txt
mgit add rest.txt >/dev/null
out="$(curl -s -X POST -H 'Content-Type: application/json' \
	-d '{"task_id":"REST-1","agent_id":"e2e-agent","message":"rest commit"}' \
	"$base/api/v1/commits")"
assert_contains "$out" '"task_id":"REST-1"' "create commit echoes the task id"
assert_contains "$out" '"message":"[MGIT:REST-1] rest commit"' "message gets the [MGIT:] task prefix"
assert_contains "$out" '"content_hash"' "commit carries the ADR-002 SHA-256 content hash"
hash="$(printf '%s' "$out" | sed -E 's/.*"commit_id":"([0-9a-f]{40})".*/\1/')"
[ "${#hash}" -eq 40 ] || _e2e_fail "could not extract commit_id from: $out"
pass "commit created: $hash"

echo "== REST: GET /api/v1/commits + /commits/:id =="
out="$(curl -s "$base/api/v1/commits")"
assert_contains "$out" "$hash" "commit list contains the new commit"
out="$(curl -s "$base/api/v1/commits/$hash")"
assert_contains "$out" "\"commit_id\":\"$hash\"" "single-commit GET returns it"
code="$(curl -s -o /dev/null -w '%{http_code}' "$base/api/v1/commits/0000000000000000000000000000000000000000")"
[ "$code" = "404" ] || _e2e_fail "unknown commit must be 404, got $code"
pass "unknown commit returns 404"

echo "== REST: GET /api/v1/tasks/:id/commits =="
out="$(curl -s "$base/api/v1/tasks/REST-1/commits")"
assert_contains "$out" "\"commit_hash\":\"$hash\"" "task index maps REST-1 to the commit"
assert_contains "$out" '"position":0' "task commit record carries its position"

echo "== REST: POST + GET /api/v1/branches =="
out="$(curl -s -X POST -H 'Content-Type: application/json' \
	-d '{"task_id":"REST-2"}' "$base/api/v1/branches")"
assert_contains "$out" '"name":"task/REST-2"' "branch create uses the task/<ID> convention"
out="$(curl -s "$base/api/v1/branches")"
assert_contains "$out" '"name":"task/REST-2"' "branch list contains the new branch"
assert_contains "$out" '"name":"main"' "branch list contains main"

echo "== REST: POST /api/v1/squash =="
out="$(curl -s -X POST -H 'Content-Type: application/json' \
	-d '{"task_id":"REST-1","message":"squashed deliverable"}' "$base/api/v1/squash")"
assert_contains "$out" '"commit_type":"squash"' "squash returns a squash-type commit"
assert_contains "$out" '"task_id":"REST-1"' "squash artifact is task-tagged"

echo "== REST: POST /api/v1/rollback =="
out="$(curl -s -X POST -H 'Content-Type: application/json' \
	-d '{"task_id":"REST-1","reason":"e2e revert"}' "$base/api/v1/rollback")"
assert_contains "$out" '"commit_type":"rollback"' "rollback returns a revert commit (append-only)"
assert_contains "$out" 'Revert: e2e revert' "revert message carries the reason"

echo "== REST: GET /api/v1/verify =="
out="$(curl -s "$base/api/v1/verify")"
assert_contains "$out" '"ok":true' "verify reports a healthy index after all operations"

echo "== REST: malformed body returns structured 400 JSON =="
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
	-d '{not json' "$base/api/v1/commits")"
[ "$code" = "400" ] || _e2e_fail "malformed body must be 400, got $code"
out="$(curl -s -X POST -H 'Content-Type: application/json' -d '{not json' "$base/api/v1/commits")"
assert_contains "$out" '"error"' "400 body is structured JSON with an error field"

# --- B) serve/CLI lock coexistence (MGIT-46) ------------------------------------
# Before MGIT-46, serve held the exclusive repo lock for its lifetime: every
# CLI command below stalled 30s and died with "another mgit process is
# running". Each must now succeed within a tight bound, with serve still up.
echo "== lock coexistence: CLI works promptly while serve runs =="
kill -0 "$server_pid" || _e2e_fail "serve died before the coexistence leg"

out="$(run_bounded 10 "mgit status under serve" -- mgit status)"
assert_not_contains "$out" "another mgit process" "status does not hit the lifetime lock"
pass "mgit status succeeded promptly"

out="$(run_bounded 10 "mgit log under serve" -- mgit log --oneline)"
assert_contains "$out" "rest commit" "log shows the REST-created commit"

printf 'cli side\n' > cli.txt
run_bounded 10 "mgit add under serve" -- mgit add cli.txt >/dev/null
out="$(run_bounded 10 "mgit commit under serve" -- mgit commit -m 'cli commit' --task-id CLI-1)"
assert_not_contains "$out" "another mgit process" "commit does not hit the lifetime lock"
assert_contains "$out" "CLI-1" "CLI commit lands task-tagged while serve runs"

out="$(run_bounded 10 "mgit worktree add under serve" -- mgit worktree add wt1 --task-id CLI-2)"
assert_contains "$out" "task CLI-2" "worktree add binds the task while serve runs"
run_bounded 10 "mgit worktree remove under serve" -- mgit worktree remove wt1 >/dev/null
pass "worktree add/remove succeeded promptly"

# ...and serve still answers afterwards (the lock is per-operation, not lost).
out="$(curl -s "$base/health")"
assert_contains "$out" '"status":"ok"' "serve still healthy after CLI activity"

# --- loopback-only bind (NFR-5.11) ---------------------------------------------
echo "== bind posture: loopback only =="
assert_contains "$(cat "$work/serve.log")" "127.0.0.1:$port" "serve announces a 127.0.0.1 bind"
lan_ip=""
if command -v ip >/dev/null 2>&1; then
	lan_ip="$(ip -4 addr 2>/dev/null | awk '/inet / && $2 !~ /^127\./ {sub(/\/.*/,"",$2); print $2; exit}')"
elif command -v ifconfig >/dev/null 2>&1; then
	lan_ip="$(ifconfig 2>/dev/null | awk '/inet / && $2 !~ /^127\./ {print $2; exit}')"
fi
if [ -n "$lan_ip" ]; then
	if curl -s --connect-timeout 2 -o /dev/null "http://$lan_ip:$port/health" 2>/dev/null; then
		_e2e_fail "REST API is reachable on non-loopback $lan_ip:$port — must bind 127.0.0.1 only"
	fi
	pass "not reachable on non-loopback interface ($lan_ip)"
else
	echo "  skip: no non-loopback IPv4 interface found (loopback-only assertion limited to the announce line)"
fi

echo "REST POSTURE E2E: PASS"
