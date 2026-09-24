#!/usr/bin/env bash
# Verify a bundled Linux sandbox daemon: <dir> holds mgit-sandboxd with lib/
# beside it, exactly as the release archive lays them out.
#
# Every check is about the machine the archive LANDS on, not the one that
# built it: the build prefix is hidden before the daemon runs, the daemon runs
# with an empty environment, and the kernel library must be found where the
# archive puts it. A negative control then removes libkrunfw and requires the
# daemon to SAY so — a check that cannot fail proves nothing.
#
# Usage: verify-linux-sandboxd.sh <dir> [<build-prefix-to-hide>] [<glibc-floor>]
# Refs: MGIT-229
set -euo pipefail

dir="$(cd "${1:?usage: verify-linux-sandboxd.sh <dir> [prefix] [glibc-floor]}" && pwd)"
hide="${2:-}"
floor="${3:-2.31}"
daemon="$dir/mgit-sandboxd"

# The binutils this reads with, overridable the conventional way (a cross
# toolchain's aarch64-linux-gnu-readelf, or a test's stand-ins: the negative
# control below is tested on any host that way). Refs: MGIT-230.9
READELF="${READELF:-readelf}"
NM="${NM:-nm}"
OBJDUMP="${OBJDUMP:-objdump}"

fail() { echo "verify-linux-sandboxd: FAIL: $*" >&2; exit 1; }
pass() { echo "  ok    $*"; }

[ -x "$daemon" ] || fail "$daemon is missing or not executable"
krun="$(ls "$dir"/lib/libkrun.so.* 2>/dev/null | head -1)"
krunfw="$(ls "$dir"/lib/libkrunfw.so.* 2>/dev/null | head -1)"
[ -n "$krun" ] || fail "no lib/libkrun.so.* beside the daemon"
[ -n "$krunfw" ] || fail "no lib/libkrunfw.so.* beside the daemon"

dyn() { "$READELF" -d "$1"; }
dyn "$daemon" | grep -q "(NEEDED).*\[$(basename "$krun")\]" || fail "the daemon does not link $(basename "$krun"):
$(dyn "$daemon" | grep NEEDED)"
dyn "$daemon" | grep -q '(RUNPATH)' && fail "the daemon carries a DT_RUNPATH; the bundle must win over LD_LIBRARY_PATH, which only DT_RPATH does"
rpath="$(dyn "$daemon" | sed -n 's/.*(RPATH).*\[\(.*\)\].*/\1/p')"
for want in '$ORIGIN/lib' '$ORIGIN/../lib/mgit'; do
	case ":$rpath:" in *":$want:"*) ;; *) fail "the daemon's DT_RPATH ($rpath) lacks $want" ;; esac
done
pass "mgit-sandboxd links $(basename "$krun") with DT_RPATH $rpath"
dyn "$krun" | grep -q '(RUNPATH)' && fail "$(basename "$krun") carries a DT_RUNPATH; its dlopen of libkrunfw must search \$ORIGIN first"
[ "$(dyn "$krun" | sed -n 's/.*(RPATH).*\[\(.*\)\].*/\1/p')" = '$ORIGIN' ] || fail "$(basename "$krun")'s DT_RPATH is not \$ORIGIN"
pass "$(basename "$krun") finds libkrunfw beside itself (DT_RPATH \$ORIGIN)"

"$NM" -D --defined-only "$krun" | grep -q ' T krun_add_net_unixgram$' ||
	fail "$(basename "$krun") was built WITHOUT networking (no krun_add_net_unixgram): every guest would fall back to TSI"
pass "libkrun exports krun_add_net_unixgram (built with NET=1)"

newest="$("$OBJDUMP" -T "$daemon" "$krun" "$krunfw" 2>/dev/null | grep -o 'GLIBC_[0-9][0-9.]*' | sed 's/GLIBC_//' | sort -uV | tail -1)"
[ -n "$newest" ] || fail "could not read the glibc symbol versions"
[ "$(printf '%s\n%s\n' "$newest" "$floor" | sort -V | tail -1)" = "$floor" ] ||
	fail "the bundle needs glibc $newest, newer than the promised floor $floor"
pass "newest glibc symbol required: $newest (floor $floor)"

hidden=""
restore() {
	[ -z "$hidden" ] || mv "$hidden" "$hide"
	[ ! -f "$krunfw.aside" ] || mv "$krunfw.aside" "$krunfw"
}
trap restore EXIT
if [ -n "$hide" ] && [ -e "$hide" ]; then
	hidden="$hide.hidden-by-verify.$$"
	mv "$hide" "$hidden"
	pass "build prefix $hide hidden for the load checks"
fi

ver="$(env -i PATH=/usr/bin:/bin "$daemon" --version 2>&1)" || fail "the daemon does not start with a clean environment: $ver"
pass "clean-environment start: $ver"

report="$(env -i PATH=/usr/bin:/bin "$daemon" --vmm 2>&1)" || fail "--vmm failed: $report"
python3 - "$report" "$(readlink -f "$krun")" "$(readlink -f "$krunfw")" <<'PY' || fail "--vmm report: $report"
import json, os, sys
r = json.loads(sys.argv[1])
want = {"libkrun": sys.argv[2], "libkrunfw": sys.argv[3]}
assert r.get("vmm") == "libkrun", f"vmm is {r.get('vmm')!r}"
assert not r.get("problems"), f"problems: {r.get('problems')}"
got = {l["name"]: os.path.realpath(l.get("path", "")) for l in r.get("libraries", [])}
for name, path in want.items():
    assert got.get(name) == path, f"{name} resolved to {got.get(name)!r}, not the bundled {path}"
PY
pass "--vmm: libkrun and libkrunfw both resolve inside the bundle, no problems"

mv "$krunfw" "$krunfw.aside"
report="$(env -i PATH=/usr/bin:/bin "$daemon" --vmm 2>&1)" || true
case "$report" in
*'"problems"'*libkrunfw*) pass "negative control: with $(basename "$krunfw") removed, --vmm names the problem" ;;
*) fail "negative control did not fire: with $(basename "$krunfw") removed --vmm said: $report" ;;
esac
mv "$krunfw.aside" "$krunfw"
echo "verify-linux-sandboxd: OK ($dir)"
