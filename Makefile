# Anamnesia developer targets.
#
# These are for working on Anamnesia. Using it needs none of them: download
# the binary and run `anamnesia setup`.
SHELL := /usr/bin/env bash

VERSION    ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
              -X main.version=$(VERSION) \
              -X main.commit=$(COMMIT) \
              -X main.date=$(BUILD_DATE)

# Where `make install` puts the binary.
PREFIX ?= /usr/local

.PHONY: help build install test fmt vet lint tidy clean release

help:
	@echo "Targets:"
	@echo "  build      build ./bin/anamnesia"
	@echo "  install    build and copy to $(PREFIX)/bin (may need sudo)"
	@echo "  test       go test ./..."
	@echo "  fmt        gofmt -s -w ."
	@echo "  vet        go vet ./..."
	@echo "  lint       fmt check + vet + test, as CI runs them"
	@echo "  tidy       go mod tidy"
	@echo "  release    cross-compile into ./dist for the supported platforms"
	@echo "  clean      remove ./bin and ./dist"

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/anamnesia ./cmd/anamnesia

install: build
	install -m 0755 bin/anamnesia $(PREFIX)/bin/anamnesia
	@echo "installed $(PREFIX)/bin/anamnesia — now run: anamnesia setup"

test:
	go test ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

# What CI enforces. Fails when anything is unformatted rather than
# reformatting it, so a pull request cannot quietly carry a format-only diff.
lint:
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi
	go vet ./...
	go test ./...

tidy:
	go mod tidy

# The binary is the whole product, so a release is just these files.
release:
	mkdir -p dist
	for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags="$(LDFLAGS)" \
	    -o dist/anamnesia-$$os-$$arch ./cmd/anamnesia || exit 1; \
	done

clean:
	rm -rf bin dist
