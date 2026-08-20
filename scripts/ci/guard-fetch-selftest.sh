#!/usr/bin/env bash
# guard-fetch-selftest.sh — prove each of guard-fetch.sh's three clauses
# against a DELIBERATELY BROKEN case. Refs: MGIT-143
#
# A guard is a claim about what happens when things go wrong, so it is worth
# exactly as much as the broken cases it has actually been run against. The
# third case below is the one that matters most: the retry it fixes was
# reviewed, merged, and wrong, and nothing caught that until it failed three
# times in production CI. It is reproduced here in miniature -- a "download"
# that truncates, an "extract" that only checks existence -- so the clause is
# proven rather than asserted.
#
# Usage: bash scripts/ci/guard-fetch-selftest.sh
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GUARD="$HERE/guard-fetch.sh"
WORK="$(mktemp -d)"
# The tripwire cases below write a probe file into the REAL tree, because a
# scanner asserted against a copy is not the scanner CI runs. The trap removes
# them even if the run is interrupted, so an abandoned probe can never be
# mistaken for a repo file.
REPO="$(cd "$HERE/../.." && pwd)"
PROBE_YML="$REPO/.github/workflows/zz-selftest-probe.yml"
PROBE_SH="$REPO/scripts/zz-selftest-probe.sh"
trap 'rm -rf "$WORK"; rm -f "$PROBE_YML" "$PROBE_SH"' EXIT

fails=0
ok() { echo "  PASS: $*"; }
bad() {
	echo "  FAIL: $*" >&2
	fails=$((fails + 1))
}
# assert_contains <file> <needle> <what>
assert_contains() {
	if grep -qF -- "$2" "$1"; then ok "$3"; else
		bad "$3 (expected to find: $2)"
		echo "    ---- captured ----" >&2
		sed 's/^/    /' "$1" >&2
	fi
}
assert_absent() {
	if grep -qF -- "$2" "$1"; then bad "$3 (unexpectedly found: $2)"; else ok "$3"; fi
}

echo "== guard-fetch self-test =="

# ---------------------------------------------------------------------------
# CLAUSE 0 (structural): the guard refuses its own misuse.
#
# -c is required because "I did not think about what a failed attempt leaves
# behind" and "nothing is left behind" must not look the same at the call
# site. That is the whole inversion: the safe state is the default and the
# exception carries its reason.
# ---------------------------------------------------------------------------
echo
echo "-- clause 0: misuse is refused, not guessed at"
out="$WORK/misuse.log"

"$GUARD" -t 5 -- true >"$out" 2>&1
[ $? -eq 2 ] && ok "a fetch with no -c is refused (exit 2)" || bad "a fetch with no -c was allowed"
assert_contains "$out" "not a retry" "the refusal explains WHY -c exists"

"$GUARD" -c none:'no artifact' -- true >"$out" 2>&1
[ $? -eq 2 ] && ok "a fetch with no -t is refused (exit 2)" || bad "a fetch with no -t was allowed"
assert_contains "$out" "measurement" "the refusal points at the measurement rule"

"$GUARD" -t 5 -c none -- true >"$out" 2>&1
[ $? -eq 2 ] && ok "'-c none' with no reason is refused" || bad "'-c none' with no reason was allowed"

"$GUARD" -t 5 -c none:'nothing written' -- true >"$out" 2>&1
[ $? -eq 0 ] && ok "a fully declared fetch runs" || bad "a fully declared fetch did not run"

# ---------------------------------------------------------------------------
# CLAUSE 1 — RETRY ERRORS, against a fetch that errors.
#
# A real fetch to a port nothing listens on: curl exits non-zero immediately,
# which is precisely the Homebrew-CDN / `curl: (18)` shape.
# ---------------------------------------------------------------------------
echo
echo "-- clause 1: a fetch that ERRORS is retried, surfaced, and fails loudly"
out="$WORK/clause1.log"
start=$(date +%s)
"$GUARD" -t 20 -b 1 -l erroring-fetch \
	-c none:'curl -o was never reached; nothing is written' -- \
	curl -fsS --max-time 5 http://127.0.0.1:1/nothing >"$out" 2>&1
rc=$?
elapsed=$(($(date +%s) - start))

[ "$rc" -ne 0 ] && ok "exhaustion fails (exit $rc), it does not fall through green" \
	|| bad "an erroring fetch exited 0"
assert_contains "$out" "attempt 1/3" "attempt 1 is surfaced"
assert_contains "$out" "attempt 2/3" "attempt 2 is surfaced"
assert_contains "$out" "attempt 3/3" "attempt 3 is surfaced"
assert_contains "$out" "retrying in 1s" "backoff after attempt 1"
assert_contains "$out" "retrying in 2s" "backoff WIDENS after attempt 2"
assert_contains "$out" "READ THIS AS AN OUTAGE, NOT A FLAKE" "exhaustion says outage, not flake"
assert_contains "$out" "::error::" "exhaustion is a GitHub error annotation, not just text"
# Three attempts must actually have been made, not one reported three times.
[ "$elapsed" -ge 3 ] && ok "three attempts really ran (${elapsed}s >= 1s+2s of backoff)" \
	|| bad "the run was too fast (${elapsed}s) to have made three attempts"

# ...and a fetch that recovers must SAY it recovered. A third-attempt success
# that reads like a first-attempt one is how an outage goes uncounted.
echo
echo "-- clause 1b: a run that only succeeds on a later attempt says so"
out="$WORK/clause1b.log"
ctr="$WORK/flaky.n"
echo 0 >"$ctr"
"$GUARD" -t 20 -b 1 -l flaky-fetch -c none:'test fixture writes nothing' -- \
	bash -c 'n=$(cat "$0"); n=$((n+1)); echo $n > "$0"; [ $n -ge 2 ]' "$ctr" >"$out" 2>&1
rc=$?
[ "$rc" -eq 0 ] && ok "the recovering fetch ultimately succeeds" || bad "the recovering fetch failed"
assert_contains "$out" "succeeded on attempt 2/3" "the log says which attempt won"
assert_contains "$out" "this run was NOT clean" "the log refuses to read as a clean run"

# ---------------------------------------------------------------------------
# CLAUSE 2 — TIMEOUT HANGS, against a fetch that hangs.
#
# The apt incident's shape: no error, no output, no exit -- just silence, for
# 5h59m52s on a step whose median is 7s. Retry alone catches none of it,
# because there is nothing to retry until something declares failure.
#
# The child assertion is not decoration. apt and curl spawn children; a kill
# that reaches only the wrapper leaves the real work running and the step
# keeps hanging while the log claims it was killed.
# ---------------------------------------------------------------------------
echo
echo "-- clause 2: a fetch that HANGS becomes a failure clause 1 can act on"
out="$WORK/clause2.log"
touched="$WORK/child-survived"
start=$(date +%s)
"$GUARD" -t 3 -b 1 -n 2 -l hanging-fetch \
	-c none:'the fixture writes only after its sleep, which never completes' -- \
	bash -c 'sleep 60 && touch "$0" & sleep 60' "$touched" >"$out" 2>&1
rc=$?
elapsed=$(($(date +%s) - start))

[ "$rc" -eq 124 ] && ok "a hang exits 124, distinct from an error" || bad "a hang exited $rc, expected 124"
[ "$elapsed" -lt 30 ] && ok "the hang was converted to a failure in ${elapsed}s, not left to the job ceiling" \
	|| bad "the hang took ${elapsed}s to be noticed"
assert_contains "$out" "HUNG" "the message says HUNG, not 'failed'"
assert_contains "$out" "no output, no error" "the message describes a hang's actual symptom"
assert_contains "$out" "attempt 2/2" "the hang was retried, so clause 1 could act on it"
assert_contains "$out" "READ THIS AS AN OUTAGE, NOT A FLAKE" "an unrecovered hang fails loudly"

# Give any surviving child the full 60s it would have needed, minus what has
# already elapsed, then assert it never fired.
sleep 3
[ ! -f "$touched" ] && ok "the hanging command's CHILD was killed too (process group, not just the pid)" \
	|| bad "a child of the hanging command survived the kill"

# ---------------------------------------------------------------------------
# CLAUSE 3 — RESTORE PRECONDITIONS, against a fetch that leaves a corrupt
# artifact.
#
# This reproduces MGIT-119's incident 4 in miniature, and it is deliberately
# run BOTH WAYS, because the lesson is not "retries work" -- it is that a
# retry without a restore is indistinguishable from one with it, right up
# until it costs you three deterministic failures and a confidently wrong
# diagnosis.
#
# The fixture is make's actual behavior: `download` is skipped when the file
# exists (make's existence check), and `extract` fails on a truncated archive.
# The third fetch is the one that would succeed -- the CDN recovers -- so the
# ONLY difference between the two runs below is whether attempt N is a real
# attempt or attempt 1 wearing a costume.
# ---------------------------------------------------------------------------
echo
echo "-- clause 3: a fetch that leaves a CORRUPT ARTIFACT"

cat >"$WORK/fake-make.sh" <<'FIXTURE'
#!/usr/bin/env bash
# Stands in for `make` in libkrunfw: fetch a tarball if it is not already
# there, then extract it. Truncates the first two transfers, succeeds on the
# third -- a transient CDN, exactly like the one that earned this clause.
set -uo pipefail
tarball="$1"; counter="$2"
if [ ! -f "$tarball" ]; then
	n=$(cat "$counter"); n=$((n + 1)); echo "$n" > "$counter"
	if [ "$n" -le 2 ]; then
		printf 'TRUNCA' > "$tarball"
		echo "curl: (18) transfer closed with 124175504 bytes remaining to read" >&2
	else
		printf 'COMPLETE-ARCHIVE' > "$tarball"
		echo "downloaded ok (transfer $n)"
	fi
else
	echo "tarball present, skipping download"   # <-- make's existence check
fi
grep -q COMPLETE-ARCHIVE "$tarball" || {
	echo "tar: Error is not recoverable: exiting now" >&2
	exit 2
}
echo "extracted ok"
FIXTURE
chmod +x "$WORK/fake-make.sh"

# --- 3a: the WRONG retry -- bounded, backed off, loud, and still useless. ---
tar1="$WORK/a.tar.xz"
ctr1="$WORK/a.n"
echo 0 >"$ctr1"
out="$WORK/clause3-nofix.log"
"$GUARD" -t 20 -b 1 -l corrupt-artifact-undeclared \
	-c none:'WRONG ON PURPOSE: this fixture DOES leave a truncated tarball behind' -- \
	"$WORK/fake-make.sh" "$tar1" "$ctr1" >"$out" 2>&1
rc=$?
downloads=$(cat "$ctr1")
[ "$rc" -ne 0 ] && ok "3a: without a restore, all three attempts fail" || bad "3a: unexpectedly succeeded"
[ "$downloads" -eq 1 ] &&
	ok "3a: the fetch actually ran ONCE in three attempts (downloads=$downloads) -- the retry was theatre" ||
	bad "3a: expected 1 real download, saw $downloads"
assert_contains "$out" "tarball present, skipping download" "3a: attempts 2 and 3 re-used the corrupt file"
assert_contains "$out" "READ THIS AS AN OUTAGE, NOT A FLAKE" \
	"3a: and it says 'outage' -- loud, well-worded, honest about the wrong thing"

# --- 3b: the SAME fetch, same fixture, with the precondition restored. ------
tar2="$WORK/b.tar.xz"
ctr2="$WORK/b.n"
echo 0 >"$ctr2"
out="$WORK/clause3-fix.log"
"$GUARD" -t 20 -b 1 -l corrupt-artifact-restored \
	-c "rm -f '$tar2'" -- \
	"$WORK/fake-make.sh" "$tar2" "$ctr2" >"$out" 2>&1
rc=$?
downloads=$(cat "$ctr2")
[ "$rc" -eq 0 ] && ok "3b: with the restore, the same fetch succeeds" || bad "3b: failed (exit $rc)"
[ "$downloads" -eq 3 ] &&
	ok "3b: three attempts meant THREE real fetches (downloads=$downloads)" ||
	bad "3b: expected 3 real downloads, saw $downloads"
assert_absent "$out" "tarball present, skipping download" "3b: no attempt re-used a corrupt file"
assert_contains "$out" "restoring preconditions" "3b: the restore is visible in the log"
assert_contains "$out" "succeeded on attempt 3/3" "3b: and the run reports that it was not clean"

# ---------------------------------------------------------------------------
# THE INVENTORY TRIPWIRE must actually catch a bare fetch.
#
# scripts/ci/fetch-inventory.sh is what turns "every fetch is guarded" from a
# claim into a property, so a scanner that quietly misses a form is worse than
# no scanner: it reports green over the exact defect it exists to find. The
# first version of it missed a single-line `run: curl ...` in a workflow, which
# is this ticket's own lesson pointed back at itself -- a guard is worth what
# its broken cases prove, not what its comments say.
#
# Each case below is a form a bare fetch really takes in this repo's files.
# ---------------------------------------------------------------------------
echo
echo "-- tripwire: the inventory scanner catches a bare fetch in every form"
SCAN="$HERE/fetch-inventory.sh"
probe="$WORK/probe"

# scan_one <file-under-.github/workflows-or-scripts> <content> <what>
# The scanner walks the real tree, so the probe is written into it and removed
# again; using a copy would test a different tree from the one CI asserts on.
scan_one() {
	local rel="$1" body="$2" what="$3" out="$WORK/scan.log"
	printf '%s\n' "$body" >"$REPO/$rel"
	if "$SCAN" >"$out" 2>&1; then
		bad "tripwire missed: $what"
		rm -f "$REPO/$rel"
		return
	fi
	rm -f "$REPO/$rel"
	ok "tripwire catches: $what"
}

scan_one '.github/workflows/zz-selftest-probe.yml' \
	'name: probe
on: workflow_dispatch
jobs:
  p:
    runs-on: ubuntu-latest
    steps:
      - name: fetch
        run: curl -fsSL https://example.com/thing -o /tmp/thing' \
	'a single-line `run: curl ...` in a workflow'

scan_one '.github/workflows/zz-selftest-probe.yml' \
	'name: probe
on: workflow_dispatch
jobs:
  p:
    runs-on: ubuntu-latest
    steps:
      - name: fetch
        run: |
          sudo apt-get install -y thing' \
	'an apt-get inside a `run: |` block'

scan_one 'scripts/zz-selftest-probe.sh' \
	'#!/usr/bin/env bash
git clone --depth 1 https://example.com/x.git /tmp/x' \
	'a git clone in a script'

scan_one 'scripts/zz-selftest-probe.sh' \
	'#!/usr/bin/env bash
out="$(wget -qO- https://example.com/x)"' \
	'a wget inside a command substitution'

# ...and it must NOT fire on a guarded one, or the tripwire becomes the thing
# people work around rather than the thing they use.
printf '%s\n' '#!/usr/bin/env bash
bash scripts/ci/guard-fetch.sh -t 120 -c "rm -f /tmp/x" -- curl -fsSL https://example.com/x -o /tmp/x' \
	>"$REPO/scripts/zz-selftest-probe.sh"
if "$SCAN" >"$WORK/scan.log" 2>&1; then
	ok "tripwire stays quiet for a guarded fetch"
else
	bad "tripwire fired on a properly guarded fetch"
	sed 's/^/    /' "$WORK/scan.log" >&2
fi
rm -f "$REPO/scripts/zz-selftest-probe.sh"

echo
if [ "$fails" -eq 0 ]; then
	echo "GUARD-FETCH SELFTEST: PASS — three clauses and the inventory tripwire, each against a broken case"
	exit 0
fi
echo "GUARD-FETCH SELFTEST: FAIL ($fails assertion(s))" >&2
exit 1
