#!/usr/bin/env bash
# Fail before building if the installed compiler differs from go.mod's sole pin.
# GOTOOLCHAIN=local measures the installed binary, never an automatic download.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
expected="$(awk '$1 == "go" { print $2 }' "$here/../../go.mod")"
[[ "$expected" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "Go version assertion: go.mod requires an exact Go patch version" >&2
    exit 1
}
actual="$(GOTOOLCHAIN=local go version)"
printf '%s\n' "$actual"
[[ "$actual" == "go version go$expected "* ]] || {
    echo "Go version assertion: expected go$expected from go.mod, installed compiler differs" >&2
    exit 1
}
