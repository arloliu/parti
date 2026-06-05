SHELL := /bin/bash

NATS_IMAGE   := $(or $(PERF_RIG_NATS_IMAGE),nats:2.12.6)
REPLICAS     := $(or $(PERF_RIG_NATS_REPLICAS),3)
COMPOSE      := docker compose -f docker/docker-compose.yaml

# Lint with the same pinned golangci-lint as the root module (../../linter.go.mod)
# against this module's .golangci.yaml (copied from root + replace-local).
LINT_TIMEOUT ?= 3m
GOLANGCI     := go tool -modfile=../../linter.go.mod golangci-lint

.PHONY: up down reset image-digest lint lint-fix vet test

## lint — run golangci-lint over the rig (same rules as the shipped library)
lint:
	$(GOLANGCI) run --timeout=$(LINT_TIMEOUT)

## lint-fix — run golangci-lint with --fix (auto-fix what it can)
lint-fix:
	$(GOLANGCI) run --fix --timeout=$(LINT_TIMEOUT)

## vet — go vet the rig
vet:
	go vet ./...

## test — race-enabled unit tests for the rig packages
test:
	go test -race ./internal/... ./cmd/...

## up — start the NATS cluster (respects PERF_RIG_NATS_REPLICAS; default 3)
up:
	$(COMPOSE) --profile r$(REPLICAS) up -d

## down — stop and remove all cluster containers (both profiles)
down:
	$(COMPOSE) --profile r3 --profile r5 down

## reset — stop containers, clear bind-mounted NATS data, and restart with a fresh cluster
## NATS writes /data as root, so the bind-mounted host dirs are root-owned and a
## plain `rm -rf` would fail with EPERM. Delete them from inside a throwaway
## root container that has the same bind mount.
reset:
	$(COMPOSE) --profile r3 --profile r5 down
	docker run --rm -v "$(CURDIR)/docker/data:/d" alpine sh -c 'rm -rf /d/perf-nats-*'
	$(COMPOSE) --profile r$(REPLICAS) up -d

## image-digest — print the resolved digest of the NATS image
image-digest:
	docker image inspect $(NATS_IMAGE) --format '{{index .RepoDigests 0}}'
