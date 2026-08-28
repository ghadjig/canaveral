# canaveral

One workspace per feature.

```
canaveral small-fixes
canaveral change-onboarding-workflow
canaveral add-tasks-per-role
canaveral onboarding/ask-for-name-at-first-step
```

Each command creates (or repairs) a fully independent universe for that feature:
its own git worktree and branch, its own ports, its own services and agent, and
its own Hyprland workspace with the windows you declared — opencode, a terminal,
tailed server logs, a browser.

```
$ canaveral small-fixes
:: norules/small-fixes  ~/projects/norules
 ok worktree ~/.local/state/canaveral/worktrees/norules/small-fixes on small-fixes
    linked node_modules
    copied .env
:: service web  bin/rails server -p 3000
 ok service web ready
 ok agent main listening on http://127.0.0.1:4096
 ok window opencode
 ok window terminal
 ok window serverlogs
 ok window chrome
 ok norules/small-fixes ready
    branch   small-fixes
    worktree ~/.local/state/canaveral/worktrees/norules/small-fixes
    ports    web=3000
```

## Commands

| Command | Purpose |
| --- | --- |
| `canaveral <feature>` | Create or repair a feature; add `--focus` to also switch to its workspace |
| `canaveral reset [feature...]` | Bring up whatever is missing; `--all` for every feature |
| `canaveral ls` | Features, branches, ports, service and window counts |
| `canaveral status [feature...]` | Per-item state, idle/worked time, CPU, memory, tokens, cost, branch status |
| `canaveral rm <feature>` | Stop everything and drop the worktree, keeping the branch |
| `canaveral attach <feature>` | Attach an opencode TUI to the feature's agent |
| `canaveral logs <feature> <name>` | Print or follow a service or agent log |
| `canaveral init` | Write a starter `canaveral.toml` |
| `canaveral hyprwatch [--install]` | Event-driven waybar refresh (see below) instead of polling |

`canaveral <feature>` and `canaveral reset` run the same reconcile pass, so both
are idempotent: run them any number of times and only the missing pieces start.

Command names are reserved and cannot be used as feature names. Use
`canaveral open <name>` if you need a feature whose name clashes.

## Namespaces

A feature name can contain `/`, like a git ref:

```
canaveral onboarding/ask-for-name
canaveral onboarding/skip-button
```

Each is still a fully independent feature — its own branch (`onboarding/ask-for-name`),
worktree, ports, services and Hyprland workspace, exactly as above. What
namespacing buys you is a shared **skill**: every feature under the same
parent path (`onboarding`, here) gets `.claude/skills/onboarding/SKILL.md`
symlinked into its worktree, pointing at the same real file. Write something
down while working on one and it's immediately visible to every sibling,
including ones you haven't created yet — no committing, no copy-pasting
context between sessions.

That symlink target lives outside any single feature's worktree, in
canaveral's own state (`~/.local/state/canaveral/skills/<project>/<namespace>/`),
so it survives `rm` and outlives every individual feature under the
namespace. `.claude/skills/<name>/SKILL.md` is a convention opencode and
Claude Code both already read natively — canaveral just keeps the file
present and linked; what's in it is entirely up to you (or the agent, if you
ask it to jot down what it learned).

Namespace sharing is scoped to the exact parent path: `onboarding/a` and
`onboarding/b/c` do **not** share a skill, even though both start with
`onboarding` — only same-parent siblings do, the same way git namespaces
nested refs.

### opencode session forking

If an agent's `tool` is `opencode`, canaveral additionally tracks the newest
session each namespaced feature's agent had, and offers it to whichever
sibling gets created next — so it isn't just working from the same *notes*
as a predecessor, it can continue the actual conversation. Opt in per window
with the `{{.Agent.<name>.Fork}}` placeholder:

```toml
[[window]]
name = "opencode"
run  = "opencode attach {{.Agent.main}} --dir {{.Worktree}} {{.Agent.main.Fork}}"
```

`{{.Agent.main.Fork}}` renders as `--session <id> --fork` when a namespace
sibling has a session to hand off (picking whichever sibling was most
recently active, whether it's still running or was already removed), or as
nothing at all otherwise — safe to include unconditionally. `--dir` matters
here: without it, a forked session keeps operating relative to wherever it
was originally started rather than this feature's own worktree. This only
ever fires for a feature's very first window; once it has a session of its
own going, later `reset`s never re-fork it and silently discard that
progress in favour of a sibling's.

## Manifest

```toml
name = "norules"
branch = "{{.Feature}}"
toolchain = "auto"          # resolve mise/asdf versions per directory

# Feature slot 0 gets the base port, slot 1 gets base+1, and so on, so several
# features run their own server at once.
[ports]
web = 3000

[database]
isolation = "shared"        # or "suffix"
# suffix_env = "DB_SUFFIX"
# setup = "bin/rails db:prepare"

# A fresh worktree holds tracked files only; bring across what the app needs.
[worktree]
link  = ["node_modules", "config/master.key"]   # shared, large
copy  = [".env", "app/assets/builds"]           # per-feature
setup = "bundle install"

[[service]]
name = "web"
cmd  = "bin/rails server -p {{.Port.web}}"
env  = { PORT = "{{.Port.web}}" }
ready.http = "{{.URL.web}}/up"
ready.timeout = "120s"

[[service]]
name = "jobs"
cmd  = "bin/jobs"
optional = true             # may fail without failing the feature

[[agent]]
name = "main"
tool = "opencode"

[[window]]
name = "opencode"
run  = "opencode attach {{.Agent.main}}"

[[window]]
name = "terminal"
run  = ""                   # a plain shell in the worktree

[[window]]
name = "serverlogs"
run  = "canaveral logs {{.Feature}} web -f"
hold = true                 # keep the pane open if the command exits

[[window]]
name = "chrome"
exec = "google-chrome --app={{.URL.web}}/"
match_class = "^Google-chrome$"

[layout]
order   = ["chrome", "opencode", "terminal", "serverlogs"]
default = [0.4, 0.2, 0.2, 0.2]
```

### Placeholders

Available in service commands, env values, readiness probes, window commands and
setup hooks:

| Placeholder | Example |
| --- | --- |
| `{{.Feature}}` `{{.Project}}` | `small-fixes`, `norules` |
| `{{.Port.web}}` | `3001` |
| `{{.URL.web}}` | `http://localhost:3001` |
| `{{.Worktree}}` `{{.Root}}` | the feature checkout, the main checkout |
| `{{.Branch}}` `{{.Slot}}` | `small-fixes`, `1` |
| `{{.Agent.main}}` | `http://127.0.0.1:4096` |
| `{{.DBSuffix}}` | `_small_fixes`, or empty when shared |

A typo such as `{{.Port.wbe}}` is a hard error at startup rather than a silently
broken URL.

Services also receive `CANAVERAL_FEATURE`, `CANAVERAL_WORKTREE`,
`CANAVERAL_PROJECT`, `CANAVERAL_ROOT` and `CANAVERAL_PORT_<NAME>`.

## Windows

`run` wraps a command in a terminal rooted at the worktree; `exec` launches a GUI
application. Terminal windows are tagged with a canaveral class, which is how
`reset` knows whether they are still open.

**GUI applications need `match_class`.** Chrome ignores `--class` on Wayland
(it is X11-only) and hands the request to an already-running browser process, so
Hyprland's exec-time workspace rule may not apply either. Its title is no help
because it follows the page as you browse. canaveral therefore identifies such a
window as *a window of this class on this feature's workspace*, which is
unambiguous because every feature owns a private workspace. To place it,
canaveral snapshots the open windows, spawns, finds the new one by difference
and moves it. Declaring `match_class` is required for `exec` windows; without it
every `reset` would spawn another copy.

Point browser windows at the application root, not at a readiness endpoint:
`/up` is a machine-facing health check that Rails renders as a blank green page.

`link` shares one copy with the main checkout; use it for large, feature-neutral
artifacts such as `node_modules`. `copy` gives the feature its own, which is what
you want for anything the feature rebuilds — sharing `app/assets/builds` would
mean whichever feature compiled last wins.

Directory copies **merge**: a tracked `.keep` makes the directory exist in the
worktree while its gitignored contents are missing, and existing files are never
overwritten, so re-provisioning cannot discard edits.

canaveral also copies `canaveral.toml` into the worktree, and every process it
spawns gets `CANAVERAL_ROOT`, so `canaveral logs ...` works from inside a
worktree even when the manifest is untracked.

## Ports and databases

Ports are derived from a stable per-feature slot, so `small-fixes` keeps `:3000`
for its whole life. Removing a feature frees its slot for the next one.

Databases default to **shared**: every feature talks to the project's normal
database, which needs no application changes. This is safe for features that do
not change the schema, but two features running migrations will corrupt each
other.

For real isolation, set `isolation = "suffix"` and make the app's database
config interpolate the suffix, for example in `config/database.yml`:

```yaml
database: <%= "norules_development#{ENV['DB_SUFFIX']}" %>
```

canaveral then exports `DB_SUFFIX=_small_fixes` and runs `database.setup` once
when the worktree is created.

## Telemetry

```
$ canaveral status small-fixes
norules/small-fixes  small-fixes  ports 3000  22 days ago
  vs origin/main: behind 4  +142/-38 across 6 file(s)
  KIND     NAME        STATE    IDLE   WORKED   CPU    MEM     TOKENS  COST   ENDPOINT
  service  web         active   -      -        9.8s   327.1M  -       -
  agent    main        waiting  -      12m34s   5.6s   546.5M  1.2k    $0.08  http://127.0.0.1:4096
  window   opencode    open     -      -        -      -       -       -
```

The branch line compares against the project's default branch (`origin/main`
if it exists, else a local `main` or `master`) at its *current* tip, so
upstream work landing after the feature branched off shows up as "behind" —
same as `git status`. `ahead`/`behind`/`diverged` and the `+insertions/-deletions`
are computed live from git, not cached.

`STATE` for an agent is one of:

| State | Meaning |
| --- | --- |
| `busy` | actively generating |
| `waiting` | idle, but a permission request (e.g. "may I run this command?") is unanswered |
| `retrying` | the provider errored and opencode is auto-retrying |
| `idle` | none of the above |

`waiting` only ever means a pending *permission* request — opencode's API has
no way to tell "the assistant asked a free-text question and stopped" apart
from plain idle, so that case isn't distinguishable.

`IDLE` is time since the last turn finished (blank while busy). `WORKED` is
the sum of actual generation time across every finished turn in the current
session — not wall-clock span, so time spent reading or away from the
keyboard doesn't count — plus a running `+12s`-style suffix for whichever
turn is currently in flight.

## On disk

```
~/.local/state/canaveral/
  features/<project>/<feature>.json   slot, branch, ports, units, windows
  logs/<project>/<feature>/*.log      service and agent logs
  worktrees/<project>/<feature>/      the feature checkout
```

Services and agents run as transient systemd user units named
`canaveral-<project>-<feature>-<svc|agent>-<name>.service`, so they survive the
terminal that started them and `systemctl --user list-units 'canaveral-*'` always
shows the truth. Removing a feature deletes its worktree but keeps the branch -
the checkout is disposable, the commits are not.

## Layout

`[layout]` gives `run`/`exec` windows a fixed column split instead of whatever
Hyprland's default tiling would produce:

```toml
[layout]
order   = ["chrome", "opencode", "terminal", "serverlogs"]
default = [0.4, 0.2, 0.2, 0.2]   # must sum to ~1.0
```

`order` fixes the left-to-right column order; `default` is each column's
fraction of the workspace width, applied the first time all of a feature's
layout windows are created together. canaveral achieves this with real dwindle
tiling — `preselect` + `splitratio exact` per window, not floating windows
pinned to computed pixel coordinates — so the columns behave like normal tiled
windows afterwards (resizable, swappable, survive a monitor change).

Dragging a window resizes the layout normally; `hyprwatch` (below) notices you
left the workspace and snapshots the resulting fractions into
`[layout.current]` in `canaveral.toml`, so the next `reset` restores what you
last had rather than the manifest's `default`. `current` is written by
canaveral, not something you're expected to hand-edit.

The ratio chain only reapplies when *every* layout window is missing (a fresh
build). If only one window died and `reset` recreates just that one, it's
spawned into whatever slot dwindle's default placement gives it, since
reflowing the other three already-placed windows around it isn't worth the
disruption.

Building a feature briefly needs to shuffle window focus to lay out each
column, which would otherwise flash across whatever workspace you're currently
looking at. If you have a second monitor, canaveral moves the new workspace
there the moment the first window exists, so all of that shuffling happens
somewhere you're not looking — your actual screen doesn't change at all during
creation. `--focus` (and `canaveral-goto` / clicking a waybar slot) explicitly
pulls the workspace back onto whichever monitor you're currently on before
switching to it. On a single-monitor machine there's nowhere to hide the work,
so it briefly flashes your current workspace and restores it afterwards
instead.

## Waybar integration

`canaveral hyprwatch` subscribes to Hyprland's event socket and signals waybar
the instant a feature workspace is created, removed, or the active workspace
changes — no polling. It sits idle at 0% CPU between events (`hyprctl`'s
`createworkspace`/`destroyworkspace`/`workspace` events only), and a 120ms
debounce collapses bursts (tearing down a feature closes several windows at
once) into a single refresh.

```
canaveral hyprwatch --install   # writes and enables a systemd --user unit
```

Pair it with waybar modules that have no `interval` and instead listen on the
matching signal. Pango markup can't round corners or pad a single text blob, so
give each feature slot its own real module (six shown; adjust to taste) instead
of one module rendering a list:

```jsonc
"custom/canaveral-1": {
  "exec": "~/.config/waybar/scripts/canaveral-ws-slot.sh 1",
  "on-click": "~/.config/hypr/bin/canaveral-goto 1",
  "signal": 8,
  "return-type": "json"
}
// ...canaveral-2 through canaveral-6, same shape, slot number changed
```

`canaveral-ws-slot.sh N` prints slot N's feature workspace as compact JSON
(`{"text": "...", "class": "active|inactive|hidden"}`); style `.active`,
`.inactive` and `.hidden` in your waybar CSS for real padding, rounded corners
and colour. `canaveral-goto N` jumps to the same workspace slot N refers to,
pulling it onto your current monitor first if canaveral built it elsewhere.

`hyprwatch` sends `SIGRTMIN+8`; `"signal": 8` in the waybar config is what maps
that back to "re-run these modules now." Change both together if you repurpose
the signal number for something else.

## Requirements

systemd user manager, git, `opencode`, and Hyprland for the window layer. Without
Hyprland the window step is skipped with a warning and everything else works.

## Testing

```
go test ./...
go test -race ./...
```
