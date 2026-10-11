PREFIX ?= $(HOME)/.local
CODEX_HOME ?= $(HOME)/.codex
VERSION ?= $(shell cat VERSION)

.PHONY: build install uninstall smoke test race test-measuretwice

build:
	go build -trimpath -ldflags "-X github.com/nibzard/looper/internal/looper.Version=$(VERSION)" -o build/looper ./cmd/looper

install:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./install.sh

uninstall:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./uninstall.sh

smoke: build
	./scripts/smoke.sh

test: build
	go test ./...
	go vet ./...
	python3 scripts/runtime-test.py
	./scripts/package-smoke.sh
	python3 scripts/skill-regression.py

race:
	go test -race ./...
	go build -race -o build/looper ./cmd/looper
	GORACE=atexit_sleep_ms=0 python3 scripts/runtime-test.py

test-measuretwice:
	node --test adapters/measuretwice/adapter.test.mjs
