# Versions

What changed in each release. Newest first.

The version here is what `canaveral --version` reports, since `scripts/build.sh`
stamps the binary from `git describe`. A build between releases reports the last
tag plus how far past it you are, like `v0.1.0-3-gbb9dacf`, so you can always map
a running binary back to an entry below.

Add to `## Unreleased` in the same commit as the change itself — see AGENTS.md.
On release, rename that heading to the new version and date, then tag it.

Categories: **Added**, **Changed**, **Fixed**, **Removed**.

## v0.2.0 — 2026-08-31

### Added

- `canaveral new <feature>` creates a feature. Creation now requires the
  keyword.
- `canaveral prune` stops leftover service and agent units whose feature no
  longer exists, with `--dry-run` to look before reaping. These are what a
  failed teardown leaves behind: a server still holding a feature's port,
  serving a worktree that has been deleted.

### Changed

- A bare `canaveral <name>` only opens a feature that already exists; it no
  longer creates one. Every other word on the command line is a command, so a
  mistyped one (`canaveral stratus` for `status`) used to silently build a
  worktree, branch, server and agent that you then had to find and tear down.
  An unknown name is now an error that suggests the command you probably meant.
- Teardown reports how many units actually stopped rather than how many it
  tried to stop, and names any it could not.

### Fixed

- Interrupting `canaveral new` left its services and agents running with
  nothing recording their existence. Three separate causes, all now fixed:
  `systemd-run` is a D-Bus client, so killing it on Ctrl-C left the unit the
  manager had already been asked to start; units were only written to state
  after the whole reconcile succeeded, so an abort lost every record of them;
  and `stop` inherited the cancelled context, which meant `exec` never ran it
  at all and every cleanup path silently did nothing. Units are recorded before
  they start, teardown ignores cancellation, and an interrupted run stops what
  it started.
- `canaveral rm` only stopped the units its state file listed, so anything
  started by an interrupted reconcile survived it — and `rm` then deleted the
  one record that could have found it. Teardown now asks systemd which units
  the feature has, and stops those too.
- A feature that failed part-way through no longer discards the record of the
  services that did start; each is saved as it comes up.
- An agent that never announced its URL was left running. Nothing could adopt
  it later, since the URL is only printed once at startup.
- A leaked unit kept the port its feature had been allocated. Because slots are
  reused, the next feature was given that same port, failed to bind, and had
  its readiness probe answered by the corpse — so it was declared ready while
  its own server was dead.

## v0.1.3 — 2026-08-31

### Fixed

- `canaveral watch` never emitted `since_prompt_seconds`. The field was
  declared and documented in the snapshot format but never assigned, so
  `omitempty` dropped it and no consumer could see how long an agent had been
  working on the current request — `canaveral status` showed it from the same
  probe all along.

## v0.1.2 — 2026-08-31

### Added

- `canaveral restart [feature] <service>...` stops and restarts named services,
  truncating the log and waiting on each one's `ready` probe. `reset` skips
  anything already running, so there was no way to pick up a code change short
  of finding the unit name and using `systemctl` — which reuses the old log and
  returns before the service is actually up. Services must be named; there is
  no "restart everything". The feature defaults to the worktree you are in, so
  `canaveral restart web jobs` works from inside one; a leading argument that is
  not a declared service is read as the feature instead.

### Fixed

- `canaveral help` described `hyprwatch` as a waybar refresh, which it stopped
  being when the waybar signalling was removed.

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
