#!/usr/bin/env python3
"""Checks on the pinned .deb lock (FEAT-6.104.1).

  guest-debs-check.py security --lock <lock> --index <Packages>
      fails, naming each package, when the security archive carries a newer
      version of a pinned package than the lock pins.
  guest-debs-check.py base --requires <file> --status <dpkg status>
      fails, naming each package, when a version the pinned packages require
      of the base image (for example perl-base) differs from the version the
      base image carries.
"""
import argparse
import re
import sys


def _order(c):
    if c == "~":
        return -1
    if c.isalpha():
        return ord(c)
    return ord(c) + 256


def _cmp_part(a, b):
    a, b = a or "", b or ""
    while a or b:
        i = 0
        while (i < len(a) and not a[i].isdigit()) or (i < len(b) and not b[i].isdigit()):
            ca = _order(a[i]) if i < len(a) and not a[i].isdigit() else 0
            cb = _order(b[i]) if i < len(b) and not b[i].isdigit() else 0
            if ca != cb:
                return -1 if ca < cb else 1
            i += 1
        a, b = a[i:], b[i:]
        ma, mb = re.match(r"\d*", a), re.match(r"\d*", b)
        na, nb = int(ma.group() or 0), int(mb.group() or 0)
        if na != nb:
            return -1 if na < nb else 1
        a, b = a[ma.end():], b[mb.end():]
    return 0


def _split(v):
    epoch = 0
    if ":" in v:
        e, v = v.split(":", 1)
        epoch = int(e)
    rev = ""
    if "-" in v:
        v, rev = v.rsplit("-", 1)
    return epoch, v, rev


def compare(a, b):
    """Debian version comparison: -1, 0 or 1."""
    ea, ua, ra = _split(a)
    eb, ub, rb = _split(b)
    if ea != eb:
        return -1 if ea < eb else 1
    return _cmp_part(ua, ub) or _cmp_part(ra, rb)


def stanzas(text):
    for blk in text.split("\n\n"):
        d, key = {}, None
        for line in blk.split("\n"):
            if line and line[0] in " \t" and key:
                d[key] += "\n" + line
            elif ":" in line:
                key, v = line.split(":", 1)
                d[key] = v.strip()
        if "Package" in d:
            yield d


def check_security(lock, index):
    pinned = {}
    for line in open(lock):
        if line.startswith("#") or not line.strip():
            continue
        pkg, ver = line.split()[:2]
        pinned[pkg] = ver
    newest = {}
    for d in stanzas(open(index).read()):
        p = d["Package"]
        if p in pinned and (p not in newest or compare(d["Version"], newest[p]) > 0):
            newest[p] = d["Version"]
    bad = [(p, pinned[p], v) for p, v in sorted(newest.items()) if compare(v, pinned[p]) > 0]
    for p, old, new in bad:
        print(f"security archive has a newer {p}: pinned {old}, security {new}", file=sys.stderr)
    return 1 if bad else 0


def check_base(requires, status):
    have = {}
    for d in stanzas(open(status).read()):
        if "installed" in d.get("Status", "") and "not-installed" not in d["Status"]:
            have[d["Package"]] = d["Version"]
    bad = 0
    for line in open(requires):
        if line.startswith("#") or not line.strip():
            continue
        pkg, want = line.split()[:2]
        got = have.get(pkg)
        if got != want:
            print(f"{pkg}: the lock needs {want}, the base image has {got}", file=sys.stderr)
            bad = 1
    return bad


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("security")
    s.add_argument("--lock", required=True)
    s.add_argument("--index", required=True)
    b = sub.add_parser("base")
    b.add_argument("--requires", required=True)
    b.add_argument("--status", required=True)
    a = ap.parse_args()
    if a.cmd == "security":
        return check_security(a.lock, a.index)
    return check_base(a.requires, a.status)


if __name__ == "__main__":
    sys.exit(main())
