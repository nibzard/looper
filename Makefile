PREFIX ?= $(HOME)/.local
CODEX_HOME ?= $(HOME)/.codex
VERSION ?= $(shell cat VERSION)
# The internal/looper suite is fsync-bound and needs several minutes. Set
# the panic budget explicitly: go test's ten-minute default leaves no
# headroom on a loaded host, so a slow-but-progressing suite aborts the
# whole run. A suite that stops making progress still fails at this bound.
# The budget starts at 30 m on a quiet host and grows with the pressure
# the run is about to join (scripts/go-test-timeout.sh applies the same
# anchors testBudget uses), because a stalled host stretches the same
# progressing suite far past a fixed budget. Pass an explicit value for a
# bounded run.
GO_TEST_TIMEOUT_MINUTES := $(shell bash scripts/go-test-timeout.sh 2>/dev/null)
GO_TEST_TIMEOUT ?= $(if $(GO_TEST_TIMEOUT_MINUTES),$(GO_TEST_TIMEOUT_MINUTES),30)m
# An explicit version pins the stamp; otherwise build-ldflags.sh derives
# a dev version from the commits since the last VERSION bump.
ifneq (,$(filter command line environment,$(origin VERSION)))
VERSION_ARG := "$(VERSION)"
else
VERSION_ARG :=
endif

# Test, vet, and race cover the module through explicit package roots,
# not ./...: a sandboxed run keeps GOMODCACHE inside the work tree under
# a looper_*/ prefix, and ./... also walks the extracted module sources
# there, which fails with "outside main module or its selected
# dependencies". Grow the roots when a new top-level package joins the
# module.
GO_PKGS := ./cmd/... ./internal/...

.PHONY: build install uninstall smoke test race test-measuretwice scrub-check

build:
	go build -trimpath -ldflags "$$(bash scripts/build-ldflags.sh $(VERSION_ARG))" -o build/looper ./cmd/looper

install:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./install.sh

uninstall:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./uninstall.sh

smoke: build
	./scripts/smoke.sh

scrub-check:
	python3 scripts/scrub-check.py

test: build
	python3 scripts/scrub-check.py
	go test -timeout $(GO_TEST_TIMEOUT) $(GO_PKGS)
	go vet $(GO_PKGS)
	python3 scripts/install-latest-test.py
	python3 scripts/skill-install-test.py
	python3 scripts/install-bin-test.py
	python3 scripts/build-test.py
	python3 scripts/runtime-test.py
	./scripts/package-smoke.sh
	python3 scripts/skill-regression.py

race:
	GORACE=atexit_sleep_ms=0 go test -race -timeout $(GO_TEST_TIMEOUT) $(GO_PKGS)
	go build -race -ldflags "$$(bash scripts/build-ldflags.sh $(VERSION_ARG))" -o build/looper ./cmd/looper
	GORACE=atexit_sleep_ms=0 python3 scripts/runtime-test.py

test-measuretwice:
	node --test adapters/measuretwice/adapter.test.mjs
