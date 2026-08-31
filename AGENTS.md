# Agent instructions

canaveral launches and controls parallel agent workspaces: git worktrees plus
transient systemd `--user` units, driven from one Go binary.

## Always

- **Update `VERSIONS.md` in the same commit as the change.** Add a bullet under
  `## Unreleased` for anything a user would notice: new commands or flags,
  changed behaviour, bugs fixed. Skip it for refactors, tests and typo fixes
  that change nothing observable. A changelog written later is a changelog
  written wrong.
- **Run `go test ./...` before committing.** `go test -race ./...` too when you
  touch anything concurrent — the watch, probe, agent and hyprevents packages
  all have goroutines in play.
- **A test that reads the environment will lie to you here.** You are running
  inside a feature worktree, so `CANAVERAL_ROOT`, `CANAVERAL_FEATURE` and the
  port variables are all set, and a test that inherits them is not testing what
  it claims. `t.Setenv` the ones your code path reads, including to `""`.
  `TestFindMissing` failed for exactly this reason and was mistaken for a
  pre-existing breakage for some time.

## Layout

`main.go` is a thin entrypoint; everything lives in `internal/`. There is no
`cmd/` directory. `internal/cli` holds one file per command and is where new
commands go — register them in `commands()` so `reserved()` picks them up
automatically, otherwise a feature named after your command becomes unreachable
by bare dispatch.

## Style

Match what is already there:

- Doc comments on exported identifiers, starting with the name.
- Comments explain *why*, not what. If the code is doing something non-obvious
  for a reason (a signal number, a path that must stay stable, an ordering
  constraint), say so.
- Errors are wrapped with context: `fmt.Errorf("resolve own path: %w", err)` —
  lowercase, no trailing punctuation.
- Stdlib first. The only third-party dependency is `BurntSushi/toml`; adding a
  second should be a deliberate decision, not a convenience.

## Building

```
scripts/build.sh      # -> ./bin/canaveral, version stamped from git describe
scripts/install.sh    # build, cycle systemd units, install to ~/.local/bin
```

Go comes from `mise` here, so `go` may not be on a bare `PATH`; the scripts fall
back to `mise exec -- go` on their own.

Installing stops running feature agents and services, including, potentially,
the one you are working inside. `scripts/install.sh` skips its own unit, but
prefer `--dry-run` first when in doubt, and `--keep-features` when the change
only affects CLI behaviour.

## Releasing

Rename `## Unreleased` in `VERSIONS.md` to the version and date, commit, then
tag `vX.Y.Z`. The tag is what `--version` reports from then on. Pre-1.0, so
breaking CLI changes bump the minor.

## Commits

Imperative mood, present tense. A `package:` prefix when the change sits in one
place (`agent: clear finished todo lists once a newer prompt makes them stale`),
plain prose when it spans several. Explain the reasoning in the body when the
change is not self-evident.
