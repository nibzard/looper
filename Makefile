PREFIX ?= $(HOME)/.local
CODEX_HOME ?= $(HOME)/.codex
VERSION ?= $(shell cat VERSION)
# An explicit version pins the stamp; otherwise build-ldflags.sh derives
# a dev version from the commits since the last VERSION bump.
ifneq (,$(filter command line environment,$(origin VERSION)))
VERSION_ARG := "$(VERSION)"
else
VERSION_ARG :=
endif

.PHONY: build install uninstall smoke test race test-measuretwice

build:
	go build -trimpath -ldflags "$$(bash scripts/build-ldflags.sh $(VERSION_ARG))" -o build/looper ./cmd/looper

install:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./install.sh

uninstall:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./uninstall.sh

smoke: build
	./scripts/smoke.sh

test: build
	go test ./...
	go vet ./...
	python3 scripts/install-latest-test.py
	python3 scripts/skill-install-test.py
	python3 scripts/build-test.py
	python3 scripts/runtime-test.py
	./scripts/package-smoke.sh
	python3 scripts/skill-regression.py

race:
	GORACE=atexit_sleep_ms=0 go test -race ./...
	go build -race -ldflags "$$(bash scripts/build-ldflags.sh $(VERSION_ARG))" -o build/looper ./cmd/looper
	GORACE=atexit_sleep_ms=0 python3 scripts/runtime-test.py

test-measuretwice:
	node --test adapters/measuretwice/adapter.test.mjs
