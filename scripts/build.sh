#!/usr/bin/env bash
# Build the canaveral binary into ./bin with a version stamped from git.
#
# The version is compiled into internal/cli.Version, so `canaveral --version`
# reports exactly which commit the installed executable came from. Set
# CANAVERAL_VERSION to override the derived value.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

out=${1:-bin/canaveral}

# derive_version prints the best available description of the working tree:
# a tag if one exists, otherwise the short commit, with -dirty appended when
# there are uncommitted changes. Falls back to "dev" outside a git checkout.
derive_version() {
	if [[ -n ${CANAVERAL_VERSION:-} ]]; then
		printf '%s\n' "$CANAVERAL_VERSION"
		return
	fi
	if git rev-parse --git-dir >/dev/null 2>&1; then
		git describe --tags --always --dirty --match 'v[0-9]*' 2>/dev/null && return
	fi
	printf 'dev\n'
}

# canaveral resolves toolchains through mise at runtime, so honour the same
# source here: a bare `go` on PATH wins, otherwise fall back to mise's.
if command -v go >/dev/null 2>&1; then
	go=(go)
elif command -v mise >/dev/null 2>&1 && mise which go >/dev/null 2>&1; then
	go=(mise exec -- go)
else
	echo "build: no go toolchain found on PATH or via mise" >&2
	exit 1
fi

version=$(derive_version)
commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

mkdir -p "$(dirname -- "$out")"

# -trimpath keeps the binary reproducible; -s -w drops the symbol table since
# nothing here needs a symbolised stack trace.
"${go[@]}" build \
	-trimpath \
	-ldflags "-s -w -X github.com/bandito/canaveral/internal/cli.Version=$version -X github.com/bandito/canaveral/internal/cli.Commit=$commit -X github.com/bandito/canaveral/internal/cli.BuildDate=$date" \
	-o "$out" \
	.

echo "built $out ($version)"
