SHELL := bash
GO ?= go
BINARY ?= bin/simulation
PKG ?= ./cmd/simulation
CONFIG ?= configs/dev.yaml

.PHONY: all build run clean quick-test

all: build

build:
	@mkdir -p $(dir $(BINARY))
	$(GO) build -o $(BINARY) $(PKG)

run: build
	./$(BINARY) --config $(CONFIG)

quick-test:
	@[ -x ./quick-test.sh ] || { echo "quick-test.sh not found or not executable"; exit 1; }
	./quick-test.sh

clean:
	rm -rf $(BINARY)
