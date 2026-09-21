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

.PHONY: help build install test test-db test-db-stop fmt vet lint tidy clean release \
        ui-install ui-dev ui-check ui-build ui-verify

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
	@echo "The console (all of these run node in a container):"
	@echo "  ui-dev     vite dev server on http://localhost:$(UI_PORT)"
	@echo "  ui-check   typecheck + eslint + vitest"
	@echo "  ui-build   rebuild the bundle the binary embeds"
	@echo "  ui-verify  fail if the committed bundle is stale"

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

# ── the console ───────────────────────────────────────────────────────
#
# Node never runs on the host: every install, build and test goes through a
# container, so a compromised package cannot reach this machine. The built
# bundle is committed under internal/ui/dist because go:embed needs it at
# compile time and `go install` has to keep working without Node.
#
# None of the Go targets depend on these. Building the binary compiles
# whatever bundle is committed, so a Go-only change needs no Node at all.
NODE_IMAGE    ?= node:22-alpine
UI_PORT       ?= 5173
ANAMNESIA_URL ?= http://host.docker.internal:8181

NODE_RUN = docker run --rm -u $(shell id -u):$(shell id -g) -e HOME=/tmp \
	-v $(CURDIR):/repo -w /repo/ui $(NODE_IMAGE)

# A sentinel rather than a phony target, so the install runs when the
# lockfile moves and not on every single build.
ui/node_modules: ui/package-lock.json ui/package.json
	$(NODE_RUN) npm ci
	@touch ui/node_modules

# Updates package-lock.json too; ui/node_modules alone does not.
ui-install:
	$(NODE_RUN) npm install

ui-dev: ui/node_modules
	docker run --rm -it -u $(shell id -u):$(shell id -g) -e HOME=/tmp \
	  -e ANAMNESIA_URL=$(ANAMNESIA_URL) -v $(CURDIR):/repo -w /repo/ui \
	  -p $(UI_PORT):5173 $(NODE_IMAGE) npm run dev

ui-check: ui/node_modules
	$(NODE_RUN) npm run typecheck
	$(NODE_RUN) npm run lint
	$(NODE_RUN) npm test

ui-build: ui/node_modules
	$(NODE_RUN) npm run build

# The committed bundle is what the binary ships, so a source change that was
# never rebuilt is a console silently lagging its own code. Vite's output is
# byte-reproducible for a fixed lockfile and node image, which is what makes
# rebuilding and diffing a gate that can actually fail.
ui-verify: ui-build
	@if ! git diff --quiet --exit-code internal/ui/dist; then \
	  echo "internal/ui/dist is stale: run 'make ui-build' and commit the result" >&2; \
	  git --no-pager diff --stat internal/ui/dist >&2; \
	  exit 1; \
	fi
	@echo "the committed console matches ui/"

