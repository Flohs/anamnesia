# Anamnesia developer targets.
SHELL := /usr/bin/env bash

VERSION    ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
              -X main.version=$(VERSION) \
              -X main.commit=$(COMMIT) \
              -X main.date=$(BUILD_DATE)

.PHONY: help build test fmt vet tidy up down logs migrate clean

help:
	@echo "Targets:"
	@echo "  build       Build the anamnesia binary into ./bin/"
	@echo "  test        go test ./..."
	@echo "  fmt         gofmt the tree"
	@echo "  vet         go vet ./..."
	@echo "  tidy        go mod tidy"
	@echo "  up          docker compose up -d (build the image first)"
	@echo "  down        docker compose down"
	@echo "  logs        docker compose logs -f anamnesia"
	@echo "  migrate     run schema migrations against the local stack"
	@echo "  clean       remove ./bin"

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/anamnesia ./cmd/anamnesia

test:
	go test ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f anamnesia

migrate:
	docker compose run --rm anamnesia migrate

clean:
	rm -rf bin
