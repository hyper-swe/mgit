# mgit — Makefile
# Safety-critical micro version control system for LLM coding agents.

.DEFAULT_GOAL := test

BINARY_NAME := mgit
BINARY_PATH := cmd/mgit/$(BINARY_NAME)
COVER_OUT   := cover.out

# Build-time version injection
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE     := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
# -X targets internal/buildinfo (NOT main): both mgit and mgit-sandboxd
# report the same build from one implementation. buildinfo.LDFlagPath is
# asserted against this line by TestLDFlagPath_MatchesTheBuildConfigs, so a
# package move cannot silently leave every binary reporting "dev". Refs: MGIT-83
BUILDINFO := github.com/hyper-swe/mgit/internal/buildinfo
LDFLAGS  := -ldflags "-X $(BUILDINFO).version=$(VERSION) -X $(BUILDINFO).commit=$(COMMIT) -X $(BUILDINFO).date=$(DATE)"

# libkrun is the DEFAULT sandbox backend on macOS (ADR-010), so mgit-sandboxd
# links it and cgo needs its pkg-config. Homebrew does not put that on the
# default search path, so derive it here rather than making every target
# depend on the developer having exported it. Core mgit is unaffected: it is
# CGO-free and never links libkrun. Refs: MGIT-61.14, ADR-010
BREW_LIBKRUN := $(shell brew --prefix libkrun 2>/dev/null)
ifneq ($(BREW_LIBKRUN),)
export PKG_CONFIG_PATH := $(BREW_LIBKRUN)/lib/pkgconfig:$(PKG_CONFIG_PATH)
endif

## build: Compile the mgit binary with version info
.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o $(BINARY_PATH) ./cmd/mgit/

## e2e: Install/posture e2e against a freshly built binary (what a user gets)
# Builds mgit into a scratch bindir (NO mgit-sandboxd — the daemon-less posture)
# and runs the core-loop, course-correction, daemon-less, REST+lock, MCP, and
# sandbox (skips without virt) e2e. This is the local mirror of the CI e2e
# jobs (MGIT-48, MGIT-53). Coverage map: docs/E2E-MATRIX.md.
.PHONY: e2e
e2e:
	@set -e; bindir="$$(mktemp -d)"; trap 'rm -rf "$$bindir"' EXIT; \
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o "$$bindir/mgit" ./cmd/mgit/; \
	echo "== core loop =="; bash scripts/e2e/core_loop.sh "$$bindir"; \
	echo "== course correction =="; bash scripts/e2e/course_correction.sh "$$bindir"; \
	echo "== daemon-less posture =="; bash scripts/e2e/daemonless_posture.sh "$$bindir"; \
	echo "== REST posture + lock coexistence =="; bash scripts/e2e/rest_posture.sh "$$bindir"; \
	echo "== MCP posture =="; MGIT_BIN="$$bindir/mgit" go run ./scripts/e2e/mcpdrive; \
	echo "== sandbox posture =="; bash scripts/e2e/sandbox_posture.sh "$$bindir"

## soak: Fleet soak + chaos gate — N concurrent sandboxes under churn
# Needs a real sandbox backend AND a guest image, so unlike `make e2e` it builds
# mgit-sandboxd too and signs it on macOS: an UNSIGNED daemon cannot create a VM
# at all and hands back a vacuous SKIP. Point it at a guest base with
# MGIT_GUEST_BASE (what `mgit sandbox base from` builds) or MGIT_GUEST_IMAGE.
# MGIT_SOAK_PROFILE=short (default, what every push runs) or long (nightly).
# Refs: MGIT-113
.PHONY: soak
soak:
	@set -e; bindir="$$(mktemp -d)"; trap 'rm -rf "$$bindir"' EXIT; \
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o "$$bindir/mgit" ./cmd/mgit/; \
	go build -trimpath $(LDFLAGS) -o "$$bindir/mgit-sandboxd" ./cmd/mgit-sandboxd/; \
	if [ "$$(uname -s)" = "Darwin" ]; then \
		codesign --force --sign - --entitlements build/darwin/vz.entitlements \
			"$$bindir/mgit-sandboxd" >/dev/null; \
	fi; \
	bash scripts/e2e/sandbox_fleet_soak.sh "$$bindir"

## soak-redteam: prove each soak invariant goes RED against a broken build
# Builds one deliberately defective mgit per invariant and requires the soak to
# fail naming that invariant. An unverified gate is a decoration. Slow (a build
# plus a full soak per case) — run it when the soak's assertions change.
# Refs: MGIT-113
.PHONY: soak-redteam
soak-redteam:
	bash scripts/e2e/fleet_soak_redteam.sh

## test: Run all unit tests
.PHONY: test
test:
	go test ./... -count=1

## test-libkrun: Build+vet+test the libkrun backend.
# On macOS libkrun is the DEFAULT backend and needs no tag; the -tags libkrun
# here is what pulls it in on LINUX, where firecracker is still the default.
# Needs libkrun installed (macOS: brew tap libkrun/krun && brew install
# libkrun; Linux: from source). Refs: ADR-010, MGIT-61.14
.PHONY: test-libkrun
test-libkrun: check-libkrun-net
	go build -tags libkrun ./...
	go vet -tags libkrun ./internal/sandboxd/backend/libkrun/ ./cmd/mgit-sandboxd/
	go test -tags libkrun ./internal/sandboxd/backend/libkrun/ -count=1

## check-libkrun-net: Assert the linked libkrun was built with networking.
# libkrun gates krun_add_net_* behind an opt-in build flag, and its header
# declares them regardless — so a libkrun built without networking fails at
# LINK time with a bare undefined-symbol error that tells an operator nothing.
# mgit attaches an explicit NIC to every sandbox in every mode (no NIC means
# TSI means full guest egress), so such a build cannot host a sandbox at all.
# This turns that into an actionable message BEFORE the build. Refs: MGIT-61.14
.PHONY: check-libkrun-net
check-libkrun-net:
	@set -e; \
	lib="$$(pkg-config --variable=libdir libkrun 2>/dev/null)"; \
	if [ -z "$$lib" ]; then \
		echo "libkrun not found via pkg-config; set PKG_CONFIG_PATH to its lib/pkgconfig" >&2; exit 1; \
	fi; \
	found=""; \
	for f in "$$lib"/libkrun.dylib "$$lib"/libkrun.so; do \
		[ -e "$$f" ] || continue; \
		if nm -g "$$f" 2>/dev/null | grep -q krun_add_net_unixgram; then found=yes; fi; \
	done; \
	if [ -z "$$found" ]; then \
		echo "" >&2; \
		echo "FATAL: the libkrun in $$lib was built WITHOUT networking support." >&2; \
		echo "" >&2; \
		echo "  mgit attaches an explicit network device to every sandbox in every" >&2; \
		echo "  mode. Without one libkrun falls back to TSI and the guest gets full" >&2; \
		echo "  host egress, so there is no safe way to build against this library." >&2; \
		echo "" >&2; \
		echo "  Fix: rebuild libkrun with 'make NET=1' (containers/libkrun), or" >&2; \
		echo "  install a package that enables it (the libkrun/krun brew tap does)." >&2; \
		echo "" >&2; \
		exit 1; \
	fi; \
	echo "libkrun networking: OK ($$lib)"

## e2e-libkrun: Boot REAL libkrun microVMs and assert the egress contract.
# Separate from test-libkrun because it needs more than the library: the
# re-exec child IS the test binary, so that binary must carry the macOS
# hypervisor entitlement — hence build, sign, run rather than `go test`.
# Skips loudly without MGIT_E2E_LIBKRUN=1. Refs: MGIT-61.10, ADR-010
.PHONY: e2e-libkrun
e2e-libkrun:
	@set -e; bin="$$(mktemp -d)/libkrun.test"; \
	export PKG_CONFIG_PATH="$$(brew --prefix libkrun)/lib/pkgconfig"; \
	export DYLD_FALLBACK_LIBRARY_PATH="$$(brew --prefix libkrunfw)/lib:$$(brew --prefix libkrun)/lib"; \
	go test -c -o "$$bin" ./internal/sandboxd/backend/libkrun/; \
	codesign --force --sign - --entitlements build/darwin/vz.entitlements "$$bin"; \
	MGIT_E2E_LIBKRUN=1 "$$bin" -test.run TestE2E_Libkrun_RealVM -test.v -test.timeout 300s

## test-race: Run tests with race detector
.PHONY: test-race
test-race:
	go test ./... -race -count=1

## test-cover: Run tests with coverage report
.PHONY: test-cover
test-cover:
	@rm -f $(COVER_OUT)
	@echo "mode: set" > $(COVER_OUT)
	@for pkg in $$(go list ./internal/...); do \
		go test -coverprofile=/tmp/mgit_cov_tmp.out -count=1 $$pkg 2>/dev/null; \
		if [ -f /tmp/mgit_cov_tmp.out ]; then \
			tail -n +2 /tmp/mgit_cov_tmp.out >> $(COVER_OUT); \
		fi; \
	done
	@go tool cover -func=$(COVER_OUT) | tail -1

## lint: Run golangci-lint per .golangci.yml
# Looks like a bare invocation with no PKG_CONFIG_PATH, but golangci-lint
# typechecks the libkrun cgo binding (the macOS default, ADR-010) and needs
# it -- the top-of-file `export PKG_CONFIG_PATH` block already covers every
# recipe in this file, this one included. Verified 2026-07-30: `make lint`
# passes with PKG_CONFIG_PATH unset beforehand, relying solely on that
# export. Noted here so this isn't mistaken for the missing-pkg-config bug
# it resembles at a glance.
# CROSS-TARGET, deliberately. golangci-lint only sees files whose build tags
# match the HOST, so a macOS developer's green `lint` never looks at
# *_linux.go — and CI lints on ubuntu. That gap let five commits reach main
# with a red CI lint nobody noticed (2026-08-12). Linting for linux here costs
# seconds and closes it. Refs: MGIT-89
.PHONY: lint
lint: lint-linux

.PHONY: lint-linux
lint-linux:
	GOOS=linux golangci-lint run

.PHONY: lint-native
lint-native:
	golangci-lint run ./...

## security-scan: Run vulnerability checker
.PHONY: security-scan
security-scan:
	govulncheck ./...

## branch-check: Refuse a branch that carries another branch's commits (BRANCH=, BASE=)
# The MGIT-142 tripwire, runnable by hand. It normally runs itself from the
# pre-push hook; this target is for checking a branch you are not standing on
# (BRANCH=feat/x) or declaring a deliberate stack (BASE=fix/parent).
.PHONY: branch-check
branch-check:
	@go run ./scripts/branchguard \
		$(if $(BRANCH),--branch $(BRANCH)) $(if $(BASE),--base $(BASE)) \
		&& echo "branch scope: clean"

## branch-survey: Run the branch-scope rule over EVERY branch and count refusals
# The measurement that keeps the guard honest: a tripwire that fires on
# legitimate work is switched off within a week, so the rule is judged by what
# it does to this repository's real branches. Refs: MGIT-142, MGIT-131
.PHONY: branch-survey
branch-survey:
	@go run ./scripts/branchguard --survey

## install-hooks: Point git at the versioned hooks in scripts/hooks
# core.hooksPath is per-clone local config, so a fresh clone needs this once.
# Refs: MGIT-142
.PHONY: install-hooks
install-hooks:
	@chmod +x scripts/hooks/*
	@git config core.hooksPath scripts/hooks
	@echo "core.hooksPath = scripts/hooks (pre-push branch-scope check active)"

## bench: Run benchmarks
.PHONY: bench
bench:
	go test ./... -bench=. -benchmem -run=^$$ -count=1

## clean: Remove build artifacts and generated files
.PHONY: clean
clean:
	rm -f $(BINARY_PATH)
	rm -f $(COVER_OUT)
	go clean -cache -testcache

## preflight: Pre-release quality gate checks
## verify-archive: Build the release archives and assert what they actually contain.
# The point is the ARTIFACT, not the build directory. dist/ always holds the
# Linux guest binaries whether or not they were packaged, and reading it is
# exactly how MGIT-65's second blocker shipped: the guest binaries were absent
# from every tarball while the build dirs looked correct. These assertions
# open the tarball. Refs: MGIT-65
.PHONY: verify-archive
verify-archive:
	@command -v goreleaser >/dev/null || { echo "goreleaser not on PATH" >&2; exit 1; }
	goreleaser release --snapshot --skip=publish --skip=validate --clean
	go test ./internal/packaging/ -run TestArchives -count=1 -v

.PHONY: preflight
preflight:
	@echo "=== mgit preflight checks ==="
	@echo ""
	@echo "[1/8] Linting..."
	@golangci-lint run ./... && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo "[2/8] Tests with race detector..."
	@go test ./... -race -count=1 && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo "[3/8] Test coverage..."
	@$(MAKE) test-cover
	@echo "[4/8] Vulnerability scan..."
	@govulncheck ./... && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo "[5/8] Build binary..."
	@$(MAKE) build && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo "[6/8] Binary smoke test..."
	@./$(BINARY_PATH) --version && echo "  PASS"
	@echo "[7/8] Anti-stub check..."
	@grep -rn '"not yet implemented"\|"not implemented"\|"integration pending"' \
		--include='*.go' --exclude='*_test.go' . && (echo "  FAIL: stubs found"; exit 1) || echo "  PASS"
	@echo "[8/8] Release archive contents..."
	@$(MAKE) verify-archive >/dev/null && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo ""
	@echo "=== All preflight checks passed ==="

## release-patch: Tag a patch release
.PHONY: release-patch
release-patch: preflight
	@CURRENT=$$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0"); \
	NEXT=$$(echo $$CURRENT | awk -F. '{print $$1"."$$2"."$$3+1}'); \
	echo "Releasing $$NEXT (was $$CURRENT)"; \
	git tag -a $$NEXT -m "Release $$NEXT"

## release-minor: Tag a minor release
.PHONY: release-minor
release-minor: preflight
	@CURRENT=$$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0"); \
	NEXT=$$(echo $$CURRENT | awk -F. '{print $$1"."$$2+1".0"}'); \
	echo "Releasing $$NEXT (was $$CURRENT)"; \
	git tag -a $$NEXT -m "Release $$NEXT"

## help: Show available targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
