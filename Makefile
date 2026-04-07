# mgit — Makefile
# Safety-critical micro version control system for LLM coding agents.
# Refs: MGIT-1.2.3, NFR-4

.DEFAULT_GOAL := test

BINARY_NAME := mgit
BINARY_PATH := cmd/mgit/$(BINARY_NAME)
COVER_OUT   := cover.out

## build: Compile the mgit binary
.PHONY: build
build:
	go build -o $(BINARY_PATH) ./cmd/mgit/

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
	go test ./... -coverprofile=$(COVER_OUT) -count=1
	go tool cover -func=$(COVER_OUT) | tail -1

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

## help: Show available targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
