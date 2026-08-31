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

## v0.1.1 — 2026-08-31

### Added

- `canaveral ws-slot [n]` maps a stable, 1-indexed slot number to a feature's
  workspace, with `--json` for waybar custom modules. Bare, it prints the whole
  mapping.
- `canaveral watch` emits `ws_slot` per feature, so a status bar can label and
  order features by the same number the jump keybinds use instead of deriving a
  position from the workspaces that happen to exist.

### Changed

- Status-bar slot numbers are stable. They were derived by sorting the Hyprland
  workspaces that existed at that instant, so slot N meant whatever sorted Nth
  right then: opening a feature that sorted earlier, or closing one, renumbered
  the bar underneath you. A feature is now assigned the lowest free number when
  created and keeps it until removed, numbered globally across projects since
  the bar shows them all at once. Existing features are assigned numbers on
  first use, ordered by creation time, so they change once and then hold.

  The waybar and keybind scripts should become thin wrappers around
  `canaveral ws-slot`, which also removes the sort logic that was duplicated
  between them — and, for a quickshell bar, a third copy reading `ws_slot`
  off the watch stream.

### Removed

- `hyprwatch` no longer signals waybar. It subscribed to Hyprland's event
  socket for two unrelated reasons; the waybar half (`SIGRTMIN+8` to every
  waybar process, a 120ms debounce, the `relevantEvents` filter and the
  install-time signal-collision check) is gone, along with the backstop signal
  `open` and `reset` sent directly. What remains is the half that earns its
  keep: recording a feature's column ratios into `[layout.current]` the moment
  you leave its workspace. A push-based bar reading `canaveral watch` never
  needed the signal.

### Fixed

- `merge` no longer refuses with "is on ... with uncommitted changes" naming the
  feature's own worktree. A feature's recorded project root was refreshed from
  wherever `canaveral.toml` was found, so any command run inside a worktree —
  where the manifest is provisioned — overwrote it with the worktree path. Merge
  then inspected the feature's own branch instead of the project's, and counted
  the provisioned manifest as uncommitted work. The main checkout is now
  resolved from git's common dir, which is correct regardless of the working
  directory, and `merge` resolves it directly rather than trusting the stored
  path, so features recorded before this fix heal without a `reset`.

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
