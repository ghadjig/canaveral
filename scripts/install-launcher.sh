#!/usr/bin/env bash
# Install the start-from-anywhere launcher into an existing quickshell config
# and print the Hyprland keybind it needs.
#
#   scripts/install-launcher.sh              install into ~/.config/quickshell/canaveral
#   scripts/install-launcher.sh --config DIR install into another quickshell config
#   scripts/install-launcher.sh --dry-run    print what would happen, change nothing
#
# The launcher is a window inside a quickshell config that is already running,
# not a shell of its own: the bar is resident anyway, so the popup opens
# instantly and inherits the same Theme.qml. That is also why this is a separate
# script from install.sh — installing canaveral should not reach into a
# quickshell config nobody asked it to touch.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

config=${XDG_CONFIG_HOME:-$HOME/.config}/quickshell/canaveral
dry_run=0

while [[ $# -gt 0 ]]; do
	case $1 in
	--config)
		config=${2:?--config needs a directory}
		shift 2
		;;
	--config=*)
		config=${1#*=}
		shift
		;;
	--dry-run)
		dry_run=1
		shift
		;;
	-h | --help)
		while IFS= read -r line; do
			[[ $line == '#!'* ]] && continue
			[[ $line == '#'* ]] || break
			printf '%s\n' "${line###}" | sed 's/^ //'
		done <"$0"
		exit 0
		;;
	*)
		echo "install-launcher: unknown argument $1" >&2
		exit 2
		;;
	esac
done

say() { printf '\033[32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m %s\n' "$*" >&2; }

src=$root/share/quickshell/LauncherWindow.qml
[[ -f $src ]] || {
	echo "install-launcher: $src is missing" >&2
	exit 1
}

[[ -d $config ]] || {
	echo "install-launcher: no quickshell config at $config" >&2
	echo "                  pass --config DIR, or create one first" >&2
	exit 1
}

# The launcher reads Theme.qml for its colours rather than carrying its own
# palette, so a config without one would load but render as unstyled defaults.
[[ -f $config/Theme.qml ]] || warn "$config has no Theme.qml; the launcher will not find its colours"

shell=$config/shell.qml
if [[ -f $shell ]] && ! grep -q 'LauncherWindow' "$shell"; then
	warn "$shell does not mention LauncherWindow"
	warn "add one instance to its ShellRoot — ONE, not one per screen:"
	warn "    LauncherWindow { }"
fi

if ((dry_run)); then
	say "would install $src -> $config/LauncherWindow.qml"
	exit 0
fi

install -m 0644 "$src" "$config/LauncherWindow.qml"
say "installed $config/LauncherWindow.qml"

cat <<'EOF'

Bind it in ~/.config/hypr/hyprland.conf:

    bind = SUPER CTRL, N, exec, qs -c canaveral ipc call launcher toggle

The letter N: SUPER+CTRL+1..9 are already the canaveral-goto jump binds.

quickshell picks the new file up on its own; if it does not, restart it.
EOF
