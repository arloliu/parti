# Makefile for mebo project
# Usage: make [target]

# Configuration
TEST_TIMEOUT    := 6m
STRESS_TIMEOUT  := 20m
LINT_TIMEOUT    := 3m
COVERAGE_DIR    := ./.coverage
COVERAGE_OUT    := $(COVERAGE_DIR)/coverage.out
COVERAGE_HTML   := $(COVERAGE_DIR)/coverage.html

# Source files
ALL_GO_FILES    := $(shell find . -name "*.go" -not -path "./vendor/*")
TEST_DIRS       := $(sort $(dir $(shell find . -name "*_test.go" -not -path "./vendor/*" -not -path "./test/integration/*" -not -path "./test/stress/*")))
INTEGRATION_DIR := ./test/integration/...
STRESS_DIR      := ./test/stress/...
LATEST_GIT_TAG  := $(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")

# Linter configuration
LINTER_GOMOD          := -modfile=linter.go.mod
GOLANGCI_LINT_VERSION := 2.5.0

# Default target
.DEFAULT_GOAL := help

.PHONY: help test test-race test-short coverage coverage-html lint fmt vet bench clean gomod-tidy update-pkg-cache ci test-unit test-integration test-stress test-all test-smoke clean-test-results test-quick

## help: Show this help message
help:
	@echo "Available targets:" && \
	grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'

## test: Run unit tests with race detector and CGO disabled
test: clean-test-results
	@echo "Running unit tests with race detector..."
	@CGO_ENABLED=1 go test $(TEST_DIRS) -timeout=$(TEST_TIMEOUT) -race || (echo "Tests failed with race detector" && exit 1)
	@echo "All unit tests passed!"

## test-unit: Run only unit tests (fast, same as test-short)
test-unit: clean-test-results
	@echo "Running unit tests..."
	@CGO_ENABLED=1 go test $(TEST_DIRS) -timeout=$(TEST_TIMEOUT) -race

## test-integration: Run only integration tests
test-integration: clean-test-results
	@echo "Running integration tests..."
	@CGO_ENABLED=1 go test $(INTEGRATION_DIR) -v -timeout=$(TEST_TIMEOUT) -race

## test-stress: Run stress and performance tests (set PARTI_STRESS=1 to enable long tests)
test-stress: clean-test-results
	@echo "Running stress tests (opt-in long tests)..."
	@CGO_ENABLED=1 PARTI_STRESS=1 go test $(STRESS_DIR) -v -timeout=$(STRESS_TIMEOUT)

## test-smoke: Run a fast stress smoke test only
test-smoke: clean-test-results
	@echo "Running stress smoke test..."
	@CGO_ENABLED=1 go test $(STRESS_DIR) -run TestStressSmoke -count=1 -timeout=2m

## test-all: Run unit, integration, and stress tests
test-all: clean-test-results
	@echo "Running all tests (unit + integration + stress)..."
	@echo "==> Running unit tests..."
	@CGO_ENABLED=1 go test $(TEST_DIRS) -timeout=$(TEST_TIMEOUT) -race
	@echo "==> Running integration tests..."
	@CGO_ENABLED=1 go test $(INTEGRATION_DIR) -v -timeout=$(TEST_TIMEOUT) -race
	@echo "==> Running stress tests..."
	@CGO_ENABLED=1 PARTI_STRESS=1 go test $(STRESS_DIR) -v -timeout=$(STRESS_TIMEOUT) -race
	@echo "All tests passed!"


test-quick: clean-test-results
	@echo "Running unit tests without race detection..."
	@CGO_ENABLED=0 go test $(TEST_DIRS) -short -timeout=$(TEST_TIMEOUT)

## coverage: Generate test coverage report (unit packages only)
coverage: clean-test-results
	@mkdir -p $(COVERAGE_DIR)
	@echo "Generating coverage report..."
	@go test $(TEST_DIRS) -coverprofile=$(COVERAGE_OUT) -covermode=atomic -timeout=$(TEST_TIMEOUT)
	@go tool cover -func=$(COVERAGE_OUT) | tail -1

## coverage-html: Generate HTML coverage report and open in browser
coverage-html: coverage
	@echo "Generating HTML coverage report..."
	@go tool cover -html=$(COVERAGE_OUT) -o $(COVERAGE_HTML)
	@echo "Coverage report generated: $(COVERAGE_HTML)"

## clean-test-results: Clean test artifacts
clean-test-results:
	@rm -f test.log *.pprof
	@rm -rf $(COVERAGE_DIR)
	@go clean -testcache

##@ Code Quality

.PHONY: linter-update linter-version
linter-update:
	@echo "Install/update linter tool..."
	@go get -tool $(LINTER_GOMOD) github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION)
	@go mod verify $(LINTER_GOMOD)

linter-version:
	@go tool $(LINTER_GOMOD) golangci-lint --version

## lint: Run linters
lint:
	@echo "Checking golangci-lint version..."
	@INSTALLED_VERSION=$$(go tool $(LINTER_GOMOD) golangci-lint --version 2>/dev/null | grep -oE 'version [^ ]+' | cut -d' ' -f2 || echo "not-installed"); \
	if [ "$$INSTALLED_VERSION" = "not-installed" ]; then \
		echo "Error: golangci-lint not found. Run 'make linter-update' to install."; \
		exit 1; \
	elif [ "$$INSTALLED_VERSION" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "Warning: golangci-lint version mismatch!"; \
		echo "  Expected: $(GOLANGCI_LINT_VERSION)"; \
		echo "  Installed: $$INSTALLED_VERSION"; \
		echo "  Run 'make linter-update' to install the correct version."; \
		exit 1; \
	else \
		echo "✓ golangci-lint $(GOLANGCI_LINT_VERSION) is installed"; \
	fi
	@echo "Running linters..."
	@go tool $(LINTER_GOMOD) golangci-lint run --timeout=$(LINT_TIMEOUT)

## fmt: Format code
fmt:
	@echo "Formatting code..."
	@gofmt -s -w .
	@goimports -w $(ALL_GO_FILES)

## vet: Run go vet
vet:
	@echo "Running go vet..."
	@go vet ./...

##@ Build & Dependencies

## gomod-tidy: Tidy go.mod and go.sum
gomod-tidy:
	@echo "Tidying go modules..."
	@go mod tidy
	@go mod verify

## update-pkg-cache: Update Go package cache with latest git tag
update-pkg-cache:
	@echo "Updating package cache with latest git tag: $(LATEST_GIT_TAG)"
	@curl -sf https://proxy.golang.org/github.com/arloliu/parti/@v/$(LATEST_GIT_TAG).info > /dev/null || \
		echo "Warning: Failed to update package cache"

##@ Cleanup

## clean: Clean all build artifacts and caches
clean: clean-test-results
	@echo "Cleaning build artifacts..."
	@go clean -cache -modcache -i -r
	@rm -rf dist/ bin/

##@ CI/CD

## ci: Run all CI checks (lint, test, coverage)
ci: lint vet test-all coverage
