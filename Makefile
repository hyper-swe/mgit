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
LDFLAGS  := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

## build: Compile the mgit binary with version info
.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o $(BINARY_PATH) ./cmd/mgit/

## e2e: Install/posture e2e against a freshly built binary (what a user gets)
# Builds mgit into a scratch bindir (NO mgit-sandboxd — the daemon-less posture)
# and runs the core-loop, daemon-less, MCP, and sandbox (skips without virt)
# e2e. This is the local mirror of the CI e2e jobs (MGIT-48).
.PHONY: e2e
e2e:
	@set -e; bindir="$$(mktemp -d)"; trap 'rm -rf "$$bindir"' EXIT; \
	CGO_ENABLED=0 go build -trimpath $(LDFLAGS) -o "$$bindir/mgit" ./cmd/mgit/; \
	echo "== core loop =="; bash scripts/e2e/core_loop.sh "$$bindir"; \
	echo "== daemon-less posture =="; bash scripts/e2e/daemonless_posture.sh "$$bindir"; \
	echo "== MCP posture =="; MGIT_BIN="$$bindir/mgit" go run ./scripts/e2e/mcpdrive; \
	echo "== sandbox posture =="; bash scripts/e2e/sandbox_posture.sh "$$bindir"

## test: Run all unit tests
.PHONY: test
test:
	go test ./... -count=1

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
.PHONY: lint
lint:
	golangci-lint run ./...

## security-scan: Run vulnerability checker
.PHONY: security-scan
security-scan:
	govulncheck ./...

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
.PHONY: preflight
preflight:
	@echo "=== mgit preflight checks ==="
	@echo ""
	@echo "[1/7] Linting..."
	@golangci-lint run ./... && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo "[2/7] Tests with race detector..."
	@go test ./... -race -count=1 && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo "[3/7] Test coverage..."
	@$(MAKE) test-cover
	@echo "[4/7] Vulnerability scan..."
	@govulncheck ./... && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo "[5/7] Build binary..."
	@$(MAKE) build && echo "  PASS" || (echo "  FAIL"; exit 1)
	@echo "[6/7] Binary smoke test..."
	@./$(BINARY_PATH) --version && echo "  PASS"
	@echo "[7/7] Anti-stub check..."
	@grep -rn '"not yet implemented"\|"not implemented"\|"integration pending"' \
		--include='*.go' --exclude='*_test.go' . && (echo "  FAIL: stubs found"; exit 1) || echo "  PASS"
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
