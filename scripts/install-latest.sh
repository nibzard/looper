#!/usr/bin/env bash
set -euo pipefail

# Keep execution in main so a truncated download cannot start installation.
main() (
    local version='' with_skills=0
    local -a install_args=()
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --version)
                [ "$#" -ge 2 ] || { echo 'Missing --version value.' >&2; return 1; }
                version="$2"; shift 2 ;;
            --with-skills) with_skills=1; shift ;;
            --bin-dir|--prefix|--skills-dir|--codex-home)
                [ "$#" -ge 2 ] || { echo "Missing $1 value." >&2; return 1; }
                install_args+=("$1" "$2"); shift 2 ;;
            -h|--help)
                cat <<'HELP'
Install the latest published Looper release from its GitHub source archive.
Requires Bash, curl, tar, Git, and Go 1.22 or later. No sudo is used.

Options:
  --version <tag>       Install a specific release, such as v0.8.3
  --bin-dir <path>      Binary directory (default: ~/.local/bin)
  --prefix <path>       Installation prefix
  --with-skills         Also install the skills bundled with that release
  --skills-dir <path>   Skill directory (used with --with-skills)
  --codex-home <path>   Codex directory (used with --with-skills)
HELP
                return 0 ;;
            *) echo "Unknown option: $1" >&2; return 1 ;;
        esac
    done
    local tool
    for tool in curl tar git go install mktemp; do
        command -v "$tool" >/dev/null 2>&1 || { echo "Required command not found: $tool" >&2; return 1; }
    done
    local repo_url='https://github.com/nibzard/looper' release_url
    if [ -z "$version" ]; then
        release_url=$(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
            --output /dev/null --write-out '%{url_effective}' "$repo_url/releases/latest")
        case "$release_url" in
            "$repo_url/releases/tag/"*) version=${release_url##*/} ;;
            *) echo "Could not resolve the latest release: $release_url" >&2; return 1 ;;
        esac
    fi
    if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?(\+[A-Za-z0-9.-]+)?$ ]]; then
        echo "Invalid release tag: $version" >&2
        return 1
    fi
    local install_temp
    install_temp=$(mktemp -d "${TMPDIR:-/tmp}/looper-install.XXXXXXXX")
    trap 'rm -rf -- "$install_temp"' EXIT
    echo "Downloading Looper $version..."
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --output "$install_temp/release.tar.gz" "$repo_url/archive/refs/tags/$version.tar.gz"
    tar -xzf "$install_temp/release.tar.gz" -C "$install_temp"
    local source_dir="$install_temp/looper-${version#v}"
    [ -f "$source_dir/install.sh" ] || { echo 'Release archive has no installer.' >&2; return 1; }
    if [ "$with_skills" -eq 0 ]; then install_args+=(--skip-skills); fi
    bash "$source_dir/install.sh" "${install_args[@]}"
    rm -rf -- "$install_temp"
    trap - EXIT
)

main "$@"
