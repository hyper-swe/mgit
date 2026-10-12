#!/usr/bin/env python3
"""Refuse execution-path Go pins that disagree with go.mod's go directive."""
import pathlib
import re
import subprocess
import sys

EXEMPT_PATHS = set()


def main():
    root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else ".").resolve()
    if EXEMPT_PATHS:
        print("Go toolchain: all execution-pin exemptions are forbidden", file=sys.stderr)
        return 1
    match = re.search(r"^go (\d+\.\d+\.\d+)\s*$", (root / "go.mod").read_text(), re.M)
    if not match:
        print("Go toolchain: missing exact go directive pin", file=sys.stderr)
        return 1
    expected = match[1]
    toolchain = re.search(r"^toolchain go(\S+)\s*$", (root / "go.mod").read_text(), re.M)
    if toolchain and toolchain[1] != expected:
        print(f"Go toolchain: go directive {expected} differs from toolchain {toolchain[1]}; go-version-file is unsafe", file=sys.stderr)
        return 1
    paths = subprocess.check_output(
        ["git", "-C", str(root), "ls-files", "-z", "--cached", "--others", "--exclude-standard"]
    ).decode().split("\0")
    failed = False
    for name in sorted(set(paths)):
        path = pathlib.Path(name)
        if not (name.startswith(("scripts/", ".github/")) or path.name.startswith(("Dockerfile", ".goreleaser")) or path.name == "Makefile"):
            continue
        file = root / name
        if not file.is_file():
            continue
        for number, line in enumerate(file.read_text(errors="replace").splitlines(), 1):
            if line.lstrip().startswith(("#", "//")):
                continue
            line = line.split("#", 1)[0]
            version = r"(\d+\.\d+(?:\.\d+)?)(?![\d.])"
            patterns = [r"go-version:\s*[\"']?" + version,
                        r"(?:MGIT_)?GO_VERSION\s*(?:[:?]?=|:)\s*[\"']?" + version,
                        r"MGIT_GO_VERSION:-" + version,
                        r"GOTOOLCHAIN=[\"']?go" + version,
                        r"golang\.org/dl/go" + version,
                        r"FROM\s+golang:" + version,
                        r"\bgo" + version]
            pins = {pin for pattern in patterns for pin in re.findall(pattern, line, re.I)}
            for pin in sorted(pins):
                where = f"{name}:{number}"
                if pin != expected:
                    print(f"Go toolchain: MISMATCH {where} pin {pin}, expected {expected}", file=sys.stderr)
                    failed = True
    if failed:
        return 1
    print(f"Go toolchain: execution pins match go{expected}; exempt paths: {len(EXEMPT_PATHS)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
