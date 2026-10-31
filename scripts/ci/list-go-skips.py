#!/usr/bin/env python3
"""List every skipped and failed test in `go test -json` output.

A skipped test is listed with the reason it gave; a failed test is listed with
the output it printed, so a red run is readable although the log is JSON.

Reads the stream on stdin. A test's skip reason is the output it printed
before its skip event, so output is grouped per package and test, and a skip
event prints the lines that mention the skip. Subtests are keyed by their full
name. Refs: MGIT-230.5.2
"""
import json
import sys

out = {}
skips = []
fails = []
for line in sys.stdin:
    try:
        ev = json.loads(line)
    except ValueError:
        continue
    key = (ev.get("Package", ""), ev.get("Test", ""))
    act = ev.get("Action")
    if act == "output":
        out.setdefault(key, []).append(ev.get("Output", "").strip())
    elif act == "skip" and key[1]:
        reason = [o for o in out.get(key, []) if o and not o.startswith(("=== ", "--- "))]
        skips.append((key, reason[-1] if reason else "(no reason printed)"))

    elif act == "fail" and key[1]:
        fails.append((key, [o for o in out.get(key, []) if o]))

print("failed tests: %d" % len(fails))
for (pkg, test), lines in fails:
    print("FAIL %s %s" % (pkg, test))
    for o in lines[-40:]:
        print("    " + o)
print("skipped tests: %d" % len(skips))
for (pkg, test), reason in skips:
    print("%s %s: %s" % (pkg, test, reason))
