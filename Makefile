PREFIX ?= $(HOME)/.local
CODEX_HOME ?= $(HOME)/.codex

.PHONY: install uninstall smoke test

install:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./install.sh

uninstall:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./uninstall.sh

smoke:
	./scripts/smoke.sh

test: smoke
