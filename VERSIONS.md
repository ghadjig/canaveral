# Versions

What changed in each release. Newest first.

The version here is what `canaveral --version` reports, since `scripts/build.sh`
stamps the binary from `git describe`. A build between releases reports the last
tag plus how far past it you are, like `v0.1.0-3-gbb9dacf`, so you can always map
a running binary back to an entry below.

Add to `## Unreleased` in the same commit as the change itself — see AGENTS.md.
On release, rename that heading to the new version and date, then tag it.

Categories: **Added**, **Changed**, **Fixed**, **Removed**.

## Unreleased

Nothing yet.

## v0.1.0 — 2026-08-31

First tagged release. Everything up to this point is the baseline: parallel
agent workspaces built on git worktrees and transient systemd user units, with
feature lifecycle (`open`, `reset`, `ls`, `status`, `rm`, `merge`), agent
attach and logs, Hyprland window management, opencode session forking, and
waybar integration via `hyprwatch` and `watch`. See README.md for what it all
does.

### Added

- `scripts/build.sh` and `scripts/install.sh`. The install script stops
  `canaveral-*` systemd user units before swapping the binary, skips the unit
  it is itself running under, and reinstalls `canaveral-hyprwatch.service` so
  its baked-in `ExecStart` path follows the new binary.
- `--version` now reports the git description, commit and build time stamped in
  at build time, instead of always saying `dev`.
