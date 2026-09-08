#!/usr/bin/env bash
# prefetch-selftest.sh — prove prefetch.sh against a server that TRUNCATES on
# purpose, both ways. Refs: MGIT-188
#
# The server answers every plain GET with the full Content-Length and then
# closes after the first CUT bytes — the shape of the 2026-08-23 mirror that
# cut linux-6.12.91.tar.xz at 68.0 M of 141 M on every attempt — and serves a
# Range request fully. So a fetch that resumes completes on its second request,
# and a fetch that restarts from zero never completes: the ONLY difference
# between the two runs below is `-C -`. A third run serves the wrong bytes
# whole, to prove the digest check deletes what it refuses.
#
# Usage: bash scripts/sandbox-image/prefetch-selftest.sh
set -uo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/sandbox-image/prefetch.sh
. "$here/prefetch.sh"
WORK="$(mktemp -d)"
server_pid=""
cleanup() { [ -n "$server_pid" ] && kill "$server_pid" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; exit 1; }
assert_contains() { grep -q -- "$2" "$1" && ok "$3" || { echo "--- output was:" >&2; cat "$1" >&2; bad "$3 (expected to see: $2)"; }; }

echo "== prefetch self-test =="
head -c 3145728 /dev/urandom > "$WORK/good.bin"   # 3 MiB, cut at 1 MiB
head -c 3145728 /dev/urandom > "$WORK/wrong.bin"
want="$(prefetch_sha256_of "$WORK/good.bin")"

python3 - "$WORK" 1048576 "$WORK/port" "$WORK/requests" <<'PY' &
import http.server, os, sys
root, cut, portfile, reqlog = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        data = open(os.path.join(root, os.path.basename(self.path)), 'rb').read()
        rng = self.headers.get('Range')
        with open(reqlog, 'a') as f: f.write(('range ' + rng) if rng else 'plain'); f.write('\n')
        if rng:
            start = int(rng.split('=')[1].split('-')[0])
            self.send_response(206)
            self.send_header('Content-Range', 'bytes %d-%d/%d' % (start, len(data) - 1, len(data)))
            self.send_header('Content-Length', str(len(data) - start)); self.end_headers()
            self.wfile.write(data[start:]); return
        self.send_response(200); self.send_header('Content-Length', str(len(data))); self.end_headers()
        self.wfile.write(data[:cut]); self.wfile.flush()
        self.connection.close()   # the mirror that closes mid-transfer
srv = http.server.HTTPServer(('127.0.0.1', 0), H)
open(portfile, 'w').write(str(srv.server_address[1]))
srv.serve_forever()
PY
server_pid=$!
disown "$server_pid" 2>/dev/null || true
for _ in $(seq 1 50); do [ -s "$WORK/port" ] && break; sleep 0.1; done
[ -s "$WORK/port" ] || bad "the truncating server did not start"
port="$(cat "$WORK/port")"
base="http://127.0.0.1:$port"

echo
echo "-- 1: WITH resume, a fetch the server cuts at 1 MiB of 3 MiB completes and verifies"
: > "$WORK/requests"
out="$WORK/resume.log"
prefetch_verified "$base/good.bin" "$want" "$WORK/out1/good.bin" 30 >"$out" 2>&1
rc=$?
[ "$rc" -eq 0 ] && ok "1: exit 0" || { cat "$out" >&2; bad "1: expected success, got exit $rc"; }
assert_contains "$out" "fetched and verified" "1: the digest was checked after the transfer"
[ "$(prefetch_sha256_of "$WORK/out1/good.bin")" = "$want" ] && ok "1: the file on disk carries the pinned digest" || bad "1: wrong bytes on disk"
grep -q "^range " "$WORK/requests" && ok "1: the server saw a Range request — the second attempt RESUMED ($(grep -c . "$WORK/requests") requests)" || bad "1: no Range request reached the server; the fetch never resumed"

echo
echo "-- 2: WITHOUT resume (the shape of the 08-23 runs), the same server never lets the fetch complete"
: > "$WORK/requests"
out="$WORK/noresume.log"
PREFETCH_NO_RESUME=1 prefetch_verified "$base/good.bin" "$want" "$WORK/out2/good.bin" 30 >"$out" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ok "2: exit $rc — the fetch failed, as the runs did" || { cat "$out" >&2; bad "2: unexpectedly succeeded without resume"; }
assert_contains "$out" "transfer closed" "2: curl reported the truncation each time"
[ ! -f "$WORK/out2/good.bin" ] && ok "2: no file was left at the destination" || bad "2: a file was left at the destination"
grep -q "^range " "$WORK/requests" && bad "2: a Range request was sent without resume" || ok "2: every request restarted from zero ($(grep -c . "$WORK/requests") requests, none ranged) — the difference between the runs is the resume alone"

echo
echo "-- 3: the wrong bytes, served whole, are refused and deleted"
out="$WORK/wrong.log"
prefetch_verified "$base/wrong.bin" "$want" "$WORK/out3/good.bin" 30 >"$out" 2>&1
rc=$?
[ "$rc" -ne 0 ] && ok "3: exit $rc" || bad "3: a wrong file was accepted"
assert_contains "$out" "does not match the pin" "3: the mismatch is named"
[ ! -f "$WORK/out3/good.bin" ] && [ ! -f "$WORK/out3/good.bin.part" ] && ok "3: nothing is left where a later existence check would find it" || bad "3: a refused file was left behind"

echo
echo "== prefetch self-test: all cases behaved =="
