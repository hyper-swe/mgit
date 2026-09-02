#!/usr/bin/env bash
# check-pinned-tools-selftest.sh — prove check-pinned-tools.sh REFUSES each
# floating-ref shape it exists for, ACCEPTS the pinned and declared shapes, and
# passes on the real tree. Refs: MGIT-180, MGIT-179
#
# A check that has never been shown to fail is a check that may be passing for
# no reason. Every refusal below is a deliberately broken fixture; the check
# must name the offending line, and must not name the clean ones.
#
# Usage: bash scripts/ci/check-pinned-tools-selftest.sh
#
# pin-exempt-file: the fixtures below are deliberately floating refs written to a scratch tree; nothing here is fetched
# fetch-guard-file: the fetch-shaped lines below are FIXTURE TEXT written to a scratch tree for the pinned-tools check to refuse; nothing in this file downloads anything
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/check-pinned-tools.sh"
REPO="$(cd "$HERE/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fails=0
ok() { echo "  PASS: $*"; }
bad() {
	echo "  FAIL: $*" >&2
	fails=$((fails + 1))
}

# fixture <name> <content...>: writes a scratch tree holding ONE workflow file
# with the given lines, and returns its root.
fixture() {
	local name=$1
	shift
	local root="$WORK/$name"
	mkdir -p "$root/.github/workflows" "$root/scripts"
	printf '%s\n' "$@" >"$root/.github/workflows/w.yml"
	echo "$root"
}

# refuses <root> <needle> <what>: the check must exit non-zero AND name the line.
refuses() {
	local out
	out="$(bash "$CHECK" "$1" 2>&1)"
	local rc=$?
	if [ $rc -ne 0 ] && grep -qF -- "$2" <<<"$out"; then ok "$3"; else
		bad "$3 (rc=$rc; expected a refusal naming: $2)"
		sed 's/^/    /' <<<"$out" >&2
	fi
}
# accepts <root> <what>
accepts() {
	local out
	out="$(bash "$CHECK" "$1" 2>&1)"
	local rc=$?
	if [ $rc -eq 0 ]; then ok "$2"; else
		bad "$2 (rc=$rc; expected acceptance)"
		sed 's/^/    /' <<<"$out" >&2
	fi
}

echo "== check-pinned-tools self-test =="

if [ ! -f "$CHECK" ]; then
	bad "the check does not exist at $CHECK"
	echo "$fails failure(s)" >&2
	exit 1
fi

echo "-- refusals: each floating-ref shape, bare --"
r=$(fixture go-latest '      run: go install golang.org/x/tools/cmd/stringer@latest')
refuses "$r" 'stringer@latest' 'go install @latest is refused'
r=$(fixture go-main '      run: go run github.com/example/tool@main ./...')
refuses "$r" 'tool@main' 'go run @main is refused'
r=$(fixture uses-branch '      - uses: someone/some-action@main')
refuses "$r" 'some-action@main' 'a uses: on a branch ref is refused'
r=$(fixture version-latest '        with:' '          version: latest')
refuses "$r" 'version: latest' 'an action input version: latest is refused'
r=$(fixture goreleaser-latest '        with:' '          goreleaser-version: latest')
refuses "$r" 'goreleaser-version: latest' 'goreleaser-version: latest is refused'
r=$(fixture latest-url '      run: curl -fsSL https://github.com/example/tool/releases/latest/download/tool.tar.gz -o t.tgz')
refuses "$r" 'releases/latest/download' 'a latest-release download URL is refused'
r=$(fixture script-latest '      run: echo ok')
printf '%s\n' '#!/usr/bin/env bash' 'go install example.com/x/cmd/y@latest' >"$r/scripts/tool.sh"
refuses "$r" 'cmd/y@latest' 'a floating ref in scripts/ is refused too'

echo "-- the refusal carries the Go-floor constraint --"
r=$(fixture floor '      run: go install github.com/goreleaser/goreleaser/v2@latest')
out="$(bash "$CHECK" "$r" 2>&1)"
if grep -qF -- 'Go floor' <<<"$out" && grep -qF -- 'MGIT-179' <<<"$out"; then
	ok 'a refusal states the Go-floor constraint and names MGIT-179'
else
	bad 'the refusal must state the Go-floor constraint (so the next pin is a decision)'
	sed 's/^/    /' <<<"$out" >&2
fi

echo "-- acceptances: pinned shapes, and a declared exception --"
r=$(fixture pinned '      - uses: actions/checkout@v4' \
	'      - uses: actions/setup-go@v5.0.2' \
	'      - uses: someone/action@0123456789abcdef0123456789abcdef01234567' \
	'      run: go install golang.org/x/tools/cmd/stringer@v0.24.0' \
	'      run: GORELEASER_VERSION=v2.17.1; go install github.com/goreleaser/goreleaser/v2@${GORELEASER_VERSION}')
accepts "$r" 'exact tags, a full SHA, an exact module version and a pinned variable are accepted'
r=$(fixture declared '          # pin-exempt: a pinned vulnerability scanner stops finding new vulnerabilities (MGIT-179)' \
	'          go install golang.org/x/vuln/cmd/govulncheck@latest')
accepts "$r" 'a floating ref with a pin-exempt declaration is accepted'
out="$(bash "$CHECK" "$r" 2>&1)"
if grep -qF -- 'DECLARED' <<<"$out" && grep -qF -- 'stops finding new vulnerabilities' <<<"$out"; then
	ok 'the declared exception is listed with its reason'
else
	bad 'a declared exception must be listed with its reason, not silently allowed'
	sed 's/^/    /' <<<"$out" >&2
fi
r=$(fixture undeclared-marker '          # pin-exempt:' \
	'          go install golang.org/x/vuln/cmd/govulncheck@latest')
refuses "$r" 'govulncheck@latest' 'a pin-exempt marker with NO reason does not declare anything'

echo "-- the real tree --"
accepts "$REPO" 'the repository itself passes (every floating ref is pinned or declared)'

if [ $fails -ne 0 ]; then
	echo "$fails failure(s)" >&2
	exit 1
fi
echo "all cases pass"
