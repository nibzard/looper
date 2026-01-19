PREFIX ?= $(HOME)/.local
CODEX_HOME ?= $(HOME)/.codex

.PHONY: install uninstall

install:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./install.sh

uninstall:
	PREFIX="$(PREFIX)" CODEX_HOME="$(CODEX_HOME)" ./uninstall.sh
