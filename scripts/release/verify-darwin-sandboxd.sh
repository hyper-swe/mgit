#!/usr/bin/env bash
# Carry a patched libkrun in the macOS build; fixes MGIT-225.
#
# Usage: verify-darwin-sandboxd.sh <dir>
#   <dir>/mgit-sandboxd, with libkrun.1.dylib in <dir>/lib (the extracted
#   archive) or <dir>/../lib/mgit (install.sh, Homebrew).
# MGIT_VERIFY_KERNEL=1 also requires libkrunfw to load, with no problem named.
# Refs: MGIT-259, MGIT-225
set -euo pipefail

dir="$(cd "${1:?usage: verify-darwin-sandboxd.sh <dir>}" && pwd)"
daemon="$dir/mgit-sandboxd"

fail() { echo "verify-darwin-sandboxd: FAIL: $*" >&2; exit 1; }
pass() { echo "  ok    $*"; }

[ -x "$daemon" ] || fail "$daemon is missing or not executable"
libdir="$dir/lib"
[ -d "$libdir" ] || libdir="$(cd "$dir/.." && pwd)/lib/mgit"
krun="$libdir/libkrun.1.dylib"
[ -f "$krun" ] || fail "no libkrun.1.dylib in $dir/lib or $libdir"

links="$(otool -L "$daemon" | tail -n +2 | awk '{print $1}')"
printf '%s\n' "$links" | grep -qx '@rpath/libkrun.1.dylib' || fail "the daemon does not link @rpath/libkrun.1.dylib:
$links"
[ "$(printf '%s\n' "$links" | grep -c libkrun)" = 1 ] || fail "the daemon links more than one libkrun:
$links"
rpaths="$(otool -l "$daemon" | awk '/cmd LC_RPATH/ { getline; getline; print $2 }')"
for want in @executable_path/lib @executable_path/../lib/mgit; do
	printf '%s\n' "$rpaths" | grep -qx "$want" || fail "the daemon's run paths lack $want: $rpaths"
done
for r in $rpaths; do
	case "$r" in @executable_path/*) ;; *) fail "the daemon carries a run path outside its own layout: $r" ;; esac
done
pass "mgit-sandboxd links @rpath/libkrun.1.dylib, run paths: $(echo $rpaths)"

[ "$(otool -D "$krun" | tail -n +2)" = @rpath/libkrun.1.dylib ] || fail "$krun's install name is $(otool -D "$krun" | tail -n +2)"
for d in $(otool -L "$krun" | tail -n +2 | awk '{print $1}'); do
	case "$d" in @rpath/libkrun.1.dylib | /usr/lib/* | /System/Library/*) ;; *) fail "libkrun.1.dylib links $d, outside the system" ;; esac
done
pass "libkrun.1.dylib is @rpath/libkrun.1.dylib and links only the system"

nm -gU "$krun" | grep -q ' _krun_add_net_unixgram$' ||
	fail "libkrun.1.dylib was built WITHOUT networking (no krun_add_net_unixgram): every guest would fall back to TSI"
pass "libkrun exports krun_add_net_unixgram (built with NET=1)"

codesign --verify --strict "$krun" || fail "libkrun.1.dylib's signature does not verify"
codesign --verify --strict "$daemon" || fail "mgit-sandboxd's signature does not verify"
codesign -d --entitlements - "$daemon" 2>/dev/null | grep -q com.apple.security.hypervisor ||
	fail "mgit-sandboxd lacks the com.apple.security.hypervisor entitlement"
pass "both signatures verify; the daemon carries com.apple.security.hypervisor"

ver="$(env -i PATH=/usr/bin:/bin "$daemon" --version 2>&1)" || fail "the daemon does not start with a clean environment: $ver"
pass "clean-environment start: $ver"

report="$(env -i PATH=/usr/bin:/bin DYLD_FALLBACK_LIBRARY_PATH=/opt/homebrew/lib "$daemon" --vmm 2>&1)" || fail "--vmm failed: $report"
python3 - "$report" "$krun" "${MGIT_VERIFY_KERNEL:-0}" <<'PY' || fail "--vmm report: $report"
import json, os, sys
r = json.loads(sys.argv[1])
assert r.get("vmm") == "libkrun", f"vmm is {r.get('vmm')!r}"
got = {l["name"]: l.get("path", "") for l in r.get("libraries", [])}
want = os.path.realpath(sys.argv[2])
assert os.path.realpath(got.get("libkrun", "")) == want, f"libkrun resolved to {got.get('libkrun')!r}, not {want}"
problems = r.get("problems") or []
if sys.argv[3] == "1":
    assert not problems, f"problems: {problems}"
    assert got.get("libkrunfw"), "libkrunfw did not resolve"
else:
    others = [p for p in problems if "libkrunfw" not in p]
    assert not others, f"problems: {others}"
    if problems:
        print("  note  libkrunfw is not installed here, so it was not checked (set MGIT_VERIFY_KERNEL=1 where it is)")
PY
pass "--vmm: libkrun resolves inside the bundle ($krun), even with /opt/homebrew/lib on the loader's fallback path"

restore() { [ ! -f "$krun.aside" ] || mv "$krun.aside" "$krun"; }
trap restore EXIT
mv "$krun" "$krun.aside"
if out="$(env -i PATH=/usr/bin:/bin "$daemon" --version 2>&1)"; then
	fail "negative control did not fire: with libkrun.1.dylib removed the daemon still started: $out"
fi
case "$out" in
*'Library not loaded: @rpath/libkrun.1.dylib'*) pass "negative control: with libkrun.1.dylib removed the daemon does not start" ;;
*) fail "negative control: the daemon failed, but not on the missing library: $out" ;;
esac
decoy="$(mktemp -d)"
cp "$krun.aside" "$decoy/libkrun.1.dylib"
report="$(env -i PATH=/usr/bin:/bin DYLD_FALLBACK_LIBRARY_PATH="$decoy" "$daemon" --vmm 2>&1)" || true
rm -rf "$decoy"
case "$report" in
*'not the copy shipped beside this daemon'*)
	pass "negative control: with it removed and a byte-identical copy on the loader's fallback path, --vmm names the problem" ;;
*) fail "negative control: with libkrun.1.dylib removed and a copy on the fallback path, --vmm did not name the problem: $report" ;;
esac
restore
echo "verify-darwin-sandboxd: OK ($dir)"
