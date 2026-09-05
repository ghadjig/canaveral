#!/usr/bin/env bash
# Build canaveral and install it onto PATH, cycling the systemd --user units
# that hold the old executable open.
#
#   scripts/install.sh                 build, stop canaveral units, install
#   scripts/install.sh --keep-features leave feature agents/services running
#   scripts/install.sh --prefix DIR    install somewhere other than ~/.local/bin
#   scripts/install.sh --no-build      install whatever is already in ./bin
#   scripts/install.sh --dry-run       print what would happen, change nothing
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

prefix=${CANAVERAL_PREFIX:-$HOME/.local/bin}
keep_features=0
do_build=1
dry_run=0
built=bin/canaveral

while [[ $# -gt 0 ]]; do
	case $1 in
	--prefix)
		prefix=${2:?--prefix needs a directory}
		shift 2
		;;
	--prefix=*)
		prefix=${1#*=}
		shift
		;;
	--keep-features)
		keep_features=1
		shift
		;;
	--no-build)
		do_build=0
		shift
		;;
	--dry-run)
		dry_run=1
		shift
		;;
	-h | --help)
		# Print the leading comment block as usage, stopping at the first
		# line of real code so the two can never drift apart.
		while IFS= read -r line; do
			[[ $line == '#!'* ]] && continue
			[[ $line == '#'* ]] || break
			printf '%s\n' "${line###}" | sed 's/^ //'
		done <"$0"
		exit 0
		;;
	*)
		echo "install: unknown argument $1" >&2
		exit 2
		;;
	esac
done

say() { printf '\033[32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m %s\n' "$*" >&2; }

have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- preflight

have go || have mise || {
	echo "install: no go toolchain found on PATH or via mise" >&2
	exit 1
}
have git || warn "git not found; canaveral needs it at runtime"
have systemctl || warn "systemctl not found; services and agents will not start"
for opt in hyprctl mise; do
	have "$opt" || warn "$opt not found; the features that use it will be skipped"
done
# Any one agent harness is enough; warning about the ones you deliberately
# do not use would be noise. See internal/agent for the full list.
have opencode || have claude ||
	warn "no coding agent found (opencode, claude); features declaring one will not start"

# ---------------------------------------------------------------- build

if ((do_build)); then
	say "building"
	"$root/scripts/build.sh" "$built"
else
	[[ -x $built ]] || {
		echo "install: $built is missing; drop --no-build" >&2
		exit 1
	}
fi
version=$("$built" --version)

# ---------------------------------------------------------------- stop units

# self_unit returns the canaveral unit this script is running under, if any.
# Installing from inside an agent is normal (canaveral builds itself in a
# worktree), and stopping that unit would kill this script mid-copy.
self_unit() {
	local cg
	cg=$(cat /proc/self/cgroup 2>/dev/null || true)
	[[ $cg =~ (canaveral-[^/]*\.service) ]] && printf '%s\n' "${BASH_REMATCH[1]}"
}

units() {
	systemctl --user list-units 'canaveral-*' --all --plain --no-legend --no-pager 2>/dev/null |
		awk '{print $1}'
}

stopped=()

if have systemctl && ! ((keep_features)); then
	mine=$(self_unit || true)
	if [[ -n $mine ]]; then
		warn "running under $mine; leaving it alone so this script survives"
	fi

	while read -r unit; do
		if [[ -z $unit ]] || [[ -n $mine && $unit == "$mine" ]]; then
			continue
		fi
		if ((dry_run)); then
			say "would stop $unit"
		else
			say "stopping $unit"
			systemctl --user stop "$unit" >/dev/null 2>&1 || warn "could not stop $unit"
		fi
		stopped+=("$unit")
	done < <(units)
fi

# canaveral-hyprwatch.service recorded dragged layout ratios back into
# canaveral.toml. That feature is gone, so retire the unit anyone who
# installed an older build still has enabled.
retire_hyprwatch() {
	have systemctl || return 0
	local unit=canaveral-hyprwatch.service
	local path=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/$unit
	[[ -e $path ]] || return 0
	if ((dry_run)); then
		say "would remove the retired $unit"
		return 0
	fi
	say "removing the retired $unit"
	systemctl --user disable --now "$unit" >/dev/null 2>&1 || true
	rm -f "$path"
	systemctl --user daemon-reload >/dev/null 2>&1 || true
}
retire_hyprwatch

# ---------------------------------------------------------------- install

dest=$prefix/canaveral

if ((dry_run)); then
	say "would install $built -> $dest"
	say "would verify $dest reports: $version"
	exit 0
fi

mkdir -p "$prefix"

# Replace via a temp file and rename so a running process never reads a
# half-written binary, and so ETXTBSY cannot bite when something still holds
# the old inode open.
tmp=$(mktemp "$prefix/.canaveral.XXXXXX")
trap 'rm -f "$tmp"' EXIT
install -m 0755 "$built" "$tmp"
mv -f "$tmp" "$dest"
trap - EXIT
say "installed $dest"

# ---------------------------------------------------------------- verify

# Verify before bringing anything back up, so a bad binary is never the one
# systemd is asked to enable.
installed=$("$dest" --version)
if [[ $installed != "$version" ]]; then
	echo "install: version mismatch: built '$version' but $dest reports '$installed'" >&2
	exit 1
fi
say "$installed"

# ---------------------------------------------------------------- restart

if have systemctl; then
	systemctl --user daemon-reload >/dev/null 2>&1 || true
fi

case ":$PATH:" in
*":$prefix:"*) ;;
*) warn "$prefix is not on PATH; add it to your shell profile" ;;
esac

if ((${#stopped[@]})); then
	warn "stopped ${#stopped[@]} feature unit(s); bring them back with 'canaveral reset <feature>'"
fi
