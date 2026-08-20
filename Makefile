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

# Throwaway Postgres for the DB-backed tests. Without a database those
# tests call t.Skip, so `go test ./...` alone reported success while
# skipping 71 of them — a green light that could not turn red. The port
# and name are deliberately not the ones `anamnesia setup` uses, so
# running the tests can never touch a real install's data.
TEST_PG_CONTAINER ?= anamnesia-test-pg
TEST_PG_PORT      ?= 5433
TEST_PG_IMAGE     ?= pgvector/pgvector:pg16
ANAMNESIA_TEST_DATABASE_URL ?= postgres://anamnesia:anamnesia-test@127.0.0.1:$(TEST_PG_PORT)/anamnesia?sslmode=disable
export ANAMNESIA_TEST_DATABASE_URL

.PHONY: help build install test test-db test-db-stop fmt vet lint tidy clean release

help:
	@echo "Targets:"
	@echo "  build      build ./bin/anamnesia"
	@echo "  install    build and copy to $(PREFIX)/bin (may need sudo)"
	@echo "  test       go test ./... against the test database"
	@echo "  test-db    start the throwaway Postgres the DB tests need"
	@echo "  test-db-stop  remove it"
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

# Depends on test-db so the DB-backed tests run instead of skipping.
test: test-db
	go test ./...

# Starts the throwaway Postgres if it is not already up, then waits for
# it to accept connections. Safe to run repeatedly.
test-db:
	@if [ -z "$$(docker ps -q -f name=^/$(TEST_PG_CONTAINER)$$)" ]; then \
	  if [ -n "$$(docker ps -aq -f name=^/$(TEST_PG_CONTAINER)$$)" ]; then \
	    docker start $(TEST_PG_CONTAINER) >/dev/null; \
	  else \
	    echo "starting $(TEST_PG_CONTAINER) on port $(TEST_PG_PORT)"; \
	    docker run -d --name $(TEST_PG_CONTAINER) \
	      -e POSTGRES_USER=anamnesia -e POSTGRES_PASSWORD=anamnesia-test \
	      -e POSTGRES_DB=anamnesia \
	      -p 127.0.0.1:$(TEST_PG_PORT):5432 $(TEST_PG_IMAGE) >/dev/null; \
	  fi; \
	fi
	@for i in $$(seq 1 30); do \
	  if docker exec $(TEST_PG_CONTAINER) pg_isready -U anamnesia -q 2>/dev/null; then exit 0; fi; \
	  sleep 1; \
	done; \
	echo "$(TEST_PG_CONTAINER) did not become ready" >&2; exit 1

test-db-stop:
	-docker rm -f $(TEST_PG_CONTAINER)

fmt:
	gofmt -s -w .

vet:
	go vet ./...

# What CI enforces. Fails when anything is unformatted rather than
# reformatting it, so a pull request cannot quietly carry a format-only diff.
lint: test-db
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
