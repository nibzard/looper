# Looper

Looper packages `looper.sh` (the Codex RALF loop runner) plus a curated set of Codex skills.

## Contents
- `bin/looper.sh`: the loop runner
- `skills/`: skills installed into `~/.codex/skills` by default
- `install.sh` / `uninstall.sh`: user-friendly installers
- `Formula/looper.rb`: Homebrew formula template

## Requirements
- `codex` CLI on PATH
- `jq` on PATH
- `git` optional (looper can init a repo if enabled)

## Install (user)
```bash
./install.sh
```

Common options:
```bash
./install.sh --codex-home ~/.codex
./install.sh --prefix /opt/looper
./install.sh --skip-skills
```

## Uninstall
```bash
./uninstall.sh
```

## Homebrew
1) Publish this repo (or a tap) and update `Formula/looper.rb` with your `url` and `sha256`.
2) Install from your tap:
```bash
brew install <tap>/looper
```
3) Install skills for Codex:
```bash
looper-install --skip-bin
```

`looper-install` defaults to installing skills in `~/.codex/skills` and the script in `~/.local/bin`.

## Usage
```bash
looper.sh
looper.sh --ls todo
looper.sh --tail --follow
```

## Configuration
The loop reads environment variables such as `CODEX_MODEL`, `CODEX_REASONING_EFFORT`,
`CODEX_YOLO`, `LOOPER_APPLY_SUMMARY`, and more. See `bin/looper.sh` for the full list.
