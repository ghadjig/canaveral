# canaveral

One workspace per feature.

```
canaveral new small-fixes
canaveral new change-onboarding-workflow
canaveral new add-tasks-per-role
canaveral new onboarding/ask-for-name-at-first-step
```

Each command creates a fully independent universe for that feature:
its own git worktree and branch, its own ports, its own services and agent, and
its own Hyprland workspace with the windows you declared — opencode, a terminal,
tailed server logs, a browser.

```
$ canaveral new small-fixes
:: norules/small-fixes  ~/projects/norules
 ok worktree ~/projects/norules/worktrees/small-fixes on small-fixes
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
    worktree ~/projects/norules/worktrees/small-fixes
    ports    web=3000
```

## Commands

| Command | Purpose |
| --- | --- |
| `canaveral new <feature>` | Create a feature; add `--focus` to also switch to its workspace |
| `canaveral <feature>` | Repair an existing feature; add `--focus` to also switch to its workspace |
| `canaveral reset [feature...]` | Bring up whatever is missing; `--all` for every feature |
| `canaveral ls` | Features, branches, ports, service and window counts |
| `canaveral status [feature...]` | Per-item state, idle/worked time, CPU, memory, tokens, cost, branch status |
| `canaveral rm [feature]` | Stop everything and drop the worktree; defaults to the feature you're in |
| `canaveral prune` | Stop leftover units whose feature no longer exists; `--dry-run` to look first |
| `canaveral rebase [feature]` | Fetch, then rebase onto the default branch; leaves conflicts in place to resolve (defaults to the one you're in) |
| `canaveral merge [feature]` | Rebase onto the default branch, merge it in, then `rm` the feature (defaults to the one you're in) |
| `canaveral attach <feature>` | Attach an opencode TUI to the feature's agent |
| `canaveral logs <feature> <name>` | Print or follow a service or agent log |
| `canaveral path [feature]` | Print a feature's worktree path; with no name, the one you are in |
| `canaveral exec <feature> -- <cmd>` | Run a command inside a feature's worktree |
| `canaveral init` | Write a starter `canaveral.toml` |
| `canaveral restart [feature] <service>...` | Stop and restart named services, waiting on their `ready` probes |
| `canaveral projects` | List the projects canaveral knows about, and where they live |
| `canaveral complete -- <words>` | Completion candidates for a partial command line, for shells and the launcher |
| `canaveral hyprwatch [--install]` | Record layout ratios when you leave a workspace (see below) |
| `canaveral ws-slot [n]` | Map a stable slot number to a feature's workspace, for status bars |
| `canaveral watch` | Stream feature/agent state as JSON for a status widget |

Creating a feature needs the `new` keyword. Everything else is a bare word, and
an unrecognised bare word used to be taken as "make me this feature" — so one
fumbled keystroke (`canaveral stratus` for `status`) would silently build a
worktree, a branch, a server and an agent that you then had to go and find.
A bare name now only ever opens something that already exists, and a near miss
says so:

```
$ canaveral stratus
canaveral: no feature "stratus" in norules
  did you mean `canaveral status`?
  create it with `canaveral new stratus`
```

Every command works on the project you are standing in. `-C` puts you in one
without moving:

```bash
canaveral -C norules ls
canaveral -C norules new small-fixes --focus
```

The name comes from the project registry (see below); a path works too. The
registry is checked first, so `-C norules` means the registered project even if
a directory called `norules` happens to sit in the one you are in.

`canaveral new`, `canaveral <feature>` and `canaveral reset` run the same
reconcile pass, so all three are idempotent: run them any number of times and only the missing pieces start.
A service that is already up is left alone, so `reset` will not pick up a code
change — `canaveral restart web` is what bounces one. It truncates the log and
waits on the manifest's `ready`, neither of which `systemctl restart` does.
Services must be named; there is no "restart everything". The feature defaults
to the worktree you are in, so name it only to reach a different one
(`canaveral restart small-fixes web`); a leading argument that is not one of the
manifest's services is read as the feature.

`canaveral rm` and `canaveral merge` both default to whichever feature's
worktree you are standing in, so finishing up is `canaveral merge` from where
you already are.

`rm` refuses a feature whose branch has not been merged into the default
branch:

```
$ canaveral rm
canaveral: mywork is not merged into main
  land it with `canaveral merge mywork`
  or discard the workspace with `canaveral rm mywork --force` (the branch is kept)
```

Committed work was never actually at risk — `rm` has always kept an unmerged
branch — but tearing down the workspace, ports and agent of something you
haven't landed leaves a branch behind that is easy to lose track of. `--force`
removes the workspace anyway and still keeps the branch; `--all` skips
unmerged features and says so rather than stopping.

Command names are reserved and cannot be used as feature names. Use
`canaveral open <name>` if you need a feature whose name clashes.

Services and agents are transient systemd units, and canaveral records each one
before asking systemd to start it, so an interrupted or failed launch is still
something `rm` knows to stop. Teardown asks systemd what is actually running
rather than trusting that record, and runs even when the context that triggered
it has already been cancelled — a Ctrl-C part-way through `canaveral new` stops
what it started rather than leaving a server holding the feature's port. If one
does escape anyway, `canaveral prune` reaps every feature unit that no feature
still claims.

## Namespaces

A feature name can contain `/`, like a git ref:

```
canaveral new onboarding/ask-for-name
canaveral new onboarding/skip-button
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

`{{.Agent.main.Fork}}` renders as `--session <id>` when a namespace
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
# Where worktrees are created. Relative paths resolve against the project
# root, so "worktrees" (the default, no need to set it) keeps them beside
# the code as ./worktrees/<feature>. Set to "state" to use canaveral's own
# state directory instead and leave the repo untouched.
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

## Getting into a worktree

Worktree paths are long, so two commands exist to avoid typing them:

```bash
cd "$(canaveral path small-fixes)"
cd "$(canaveral path)"                       # the feature you're already in
canaveral exec small-fixes -- git rebase main
canaveral exec small-fixes -- bin/rails test
```

`canaveral path` with no feature answers "where am I", using three signals in
order: the working directory sitting inside a worktree, then
`$CANAVERAL_FEATURE` (canaveral exports it into every process it starts, so a
terminal it opened still knows which feature it belongs to after you have cd'd
away), then the focused Hyprland workspace (which catches a terminal you opened
yourself on a feature's workspace, inheriting neither). It needs no project in
scope — the workspace name carries the project and the registry turns that into
a checkout — so it works from anywhere.

That last signal is used only by `path`. `merge` and `restart` stop at the first
two: a workspace is a property of the window, not the shell, and windows get
dragged between workspaces. Pointing `cd` at the wrong directory is a keystroke
to undo; merging the wrong feature is not.

`exec` runs in the worktree with the same toolchain and environment the
feature's own services get, and exits with the command's own status, so it
composes in scripts.

Worktrees live beside the code by default (`./worktrees/<feature>`), so add
`worktrees/` to `.gitignore`: `git status` will otherwise show them, though
`git clean -xdf` is safe (it skips nested repositories) and `rg`/`git grep`
respect the ignore. Non-gitignore-aware tools such as plain `grep -r` will
still descend into every feature's copy. Set `[worktree] root = "state"` to
put them in canaveral's own state directory instead and leave the repo
completely untouched.

For a shell shortcut with tab-completion, in `~/.bash_aliases`:

```bash
cv() {
    local d
    if [ -n "$1" ]; then
        d=$(canaveral path "$1") || return
    else
        # Bare `cv` goes to the root of wherever you are: the worktree of the
        # feature you're in, or the project's main checkout if you're not in
        # one. Both are "up and out of here", which is the only thing anyone
        # means by it.
        d=$(canaveral path 2>/dev/null) ||
            d=$(dirname "$(cd "$(git rev-parse --git-common-dir 2>/dev/null)" 2>/dev/null && pwd)") ||
            return
    fi
    cd "$d"
}
```

Tab-completion for `canaveral` itself (and for `cv`, if it is defined) comes
from `share/completions/canaveral.bash`:

```bash
source /path/to/canaveral/share/completions/canaveral.bash
```

It completes commands, features, namespaces one segment at a time, service and
agent names, flags, and project names after `-C`. All of it comes from
`canaveral complete`, which the launcher popup calls too, so the shell and the
popup can never disagree about what a half-typed line means.

## The project registry

canaveral needs to find a project without being inside it — for `-C`, and for
the launcher, which has no meaningful working directory at all. It keeps an
index at `~/.local/state/canaveral/projects.json`.

Nothing has to be maintained: a project registers itself the first time any
command resolves its manifest, and projects that predate the registry are
recovered from their features' own state records. Use one and it is addressable
by name forever after.

```bash
canaveral projects                    # name, root, when it was last used
canaveral projects --scan ~/code      # register everything under a directory
canaveral projects --add ~/work/thing # register one checkout
canaveral projects --prune            # drop entries whose checkout is gone
canaveral projects --forget norules   # drop one, leaving the checkout alone
```

`--scan` stops at the first `canaveral.toml` it finds down any path and skips
linked git worktrees, so it will not register a project once per feature —
canaveral copies the manifest into every worktree it provisions, and a naive
walk would find one "project" per worktree, all claiming the same name.

A name is also the project's key in the state directory, so two checkouts
calling themselves the same thing already share each other's features. The
registry refuses to record the second and says so rather than quietly
repointing.

## The launcher

`share/quickshell/LauncherWindow.qml` is a popup for starting anything from
anywhere: type a project, then a command.

```
no              ->  norules
<Tab>           ->  attach  complete  exec  init  logs  ls  merge  new  …
rm              ->  workflows/
<Tab>           ->  workflows/insurance-application-for-dependants
```

Creating goes through the same `new` keyword the CLI requires, and the popup
completes namespaces for it while refusing to offer names that already exist:

```
norules new             ->  childcare-allowance/  documents/  leaves/  onboarding/
work                    ->  workflows/
<Tab> shiny-thing       ->  shiny-thing   (create this feature)
```

The namespaces offered there are every namespace the project has ever had, not
just the ones with a feature open right now. A namespace's shared skill and
recorded sessions outlive the features that wrote them — that is the whole
reason they live outside any worktree — so the namespace with the most
accumulated knowledge is exactly the one you want one keystroke away when
starting the next feature under it, not the one you have to retype from memory.

The line is `<project> <argv>`, and `<argv>` is an ordinary canaveral command
line — the whole thing maps onto `canaveral -C <project> <argv>`, which is shown
in the popup's footer before you run it. Tab completes, Enter runs (completing
first if the highlighted candidate merely extends what you typed, so Enter can
never fire a half-typed name), Esc closes. `rm` and `merge` ask for a second
Enter.

It installs into an existing quickshell config as one more window, rather than
being a shell of its own — the bar is resident anyway, so the popup opens
instantly and inherits the same `Theme.qml`:

```bash
scripts/install-launcher.sh          # -> ~/.config/quickshell/canaveral/
```

Add one instance to that config's `shell.qml` — one, not one per screen; it
moves itself to the focused monitor and takes an exclusive keyboard grab:

```qml
LauncherWindow {}
```

And bind it in `hyprland.conf`:

```
bind = SUPER CTRL, N, exec, qs -c canaveral ipc call launcher toggle
```

The letter N because `SUPER+CTRL+1..9` are already the `canaveral-goto` jump
binds. The keybind talks to the running bar over IPC and starts nothing, so the
popup is instant.

The QML holds no knowledge of the grammar: it draws a text box, shells out to
`canaveral complete --launcher`, and renders the JSON. If a completion is wrong,
the bug is in Go, where there is a test for it.

Words are passed after `--`, the one being completed last — with a trailing
empty word when the line ends in a space, exactly as bash's `COMP_WORDS` does:

```jsonc
$ canaveral complete --launcher -- norules rm ''
{
  "prefix": "",
  "common": "workflows/",             // what Tab should insert
  "candidates": [
    { "value": "workflows/",          // project|command|feature|namespace|
      "kind": "namespace",            //   service|agent|flag|new
      "desc": "1 feature",
      "continues": true }             // a prefix, not an answer: do not end the word
  ],
  "project": "norules",
  "command": "rm",                    // "open" when a bare feature name is dispatching
  "destructive": true,                // confirm before running this
  "fuzzy": false,                     // nothing matched the prefix; these are substring guesses
  "error": ""                         // reported in-band: a completer must not exit non-zero
}
```

`--format=lines` prints bare values instead, for shells that filter and render
their own.

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
| `waiting` | blocked on you: a question it asked, or a permission request |
| `busy` | actively generating |
| `retrying` | the provider errored and opencode is auto-retrying |
| `idle` | none of the above |

A blocked **subagent** counts too: the Task tool runs subagents in their own
sessions, and one stopping for permission blocks the whole conversation,
since the parent's turn is sitting waiting for its result.

`waiting` covers both things that block an agent on you: a **question** (the
assistant asked you something and stopped) and a **permission** request
("may I run this command?"). It deliberately outranks `busy`, because
opencode keeps a session's own status "busy" while a question or permission
sits unanswered — the turn hasn't ended, it's blocked inside a tool call —
but an agent that cannot move without you is the more useful thing to
report.

The `agent` summary line above the table also shows the model and its
reasoning effort, the agent's own task list when it is using one
(`todo 6/9`), the tool call in flight, and the last thing said on each side
of the conversation:

```
  agent main: waiting · worked 59m27s · on this 5m2s · claude-sonnet-5 (high) · 1 session(s) +5 subagent(s)
    needs: permission: external_directory [/home/…/gems/ruby_llm/*]
    task: Add a smoke test
    now:  bash: bin/rails test
    you:  cancel workflow shouldn't be here
    said: Confirmed fixed. Here's a fresh link with the corrected data: http://localhost:…
```

The `task:` line is the task — the only real progress signal opencode and Claude Code
expose.

`IDLE` is time since the last turn finished (blank while busy). `WORKED` is
the sum of actual generation time across every finished turn in the current
session — not wall-clock span, so time spent reading or away from the
keyboard doesn't count — and is cumulative across every prompt in that
session.

`on this` is how long since **your** most recent message: the answer to "how
long has it been working on what I asked". It is worth distinguishing from
the per-message generation timer, which is not shown: a single prompt
produces one assistant message per tool round trip, so that timer resets
every few seconds and reads like a command timer without being one. It
remains available as `working_nanos` in `--json` and `since_prompt_seconds`
in `canaveral watch`.

## Watching from a status widget

`canaveral watch` streams one JSON snapshot per line to stdout, emitting only
when something actually changes:

```
canaveral watch          # this project
canaveral watch --all    # every project
```

```jsonc
{
  "time": "2026-08-28T12:41:54+03:00",
  "features": [{
    "project": "norules", "name": "small-fixes", "key": "norules/small-fixes",
    "branch": "small-fixes", "workspace": "norules:small-fixes",
    "ws_slot": 1,                     // stable widget slot, matches `canaveral ws-slot`
    "status": "waiting",              // waiting|error|retrying|working|idle|offline
    "since": "2026-08-28T12:39:06+03:00",
    "created_at": "2026-08-28T11:02:11+03:00",
    "agents": [{
      "name": "main", "status": "waiting", "url": "http://127.0.0.1:4096",
      "model": "claude-sonnet-5",
      "variant": "high",            // provider-specific reasoning effort
      "provider": "github-copilot",
      "subagents": 3,               // spawned sessions, already folded into the totals
      "tokens": 8068183, "cost": 3.56,
      "worked_seconds": 92.4,
      "pending": {                    // present only while status is "waiting"
        "kind": "question",           // question | permission
        "header": "Fork trigger",     // short label, <=30 chars
        "detail": "When should canaveral fork a session?",
        "options": ["Always", "Never"]
      }
    }],
    "git": {                          // branch position; measured slowly, see below
      "base": "origin/main",
      "ahead": 6, "behind": 0,
      "files_changed": 9, "insertions": 148, "deletions": 32,
      "uncommitted": 2                // working-tree changes, staged or not
    }
  }],
  "summary": {
    "total": 3, "needs_attention": 1,
    "waiting": 1, "working": 1, "idle": 1, "errored": 0,
    "status": "waiting",
    "oldest_attention_since": "2026-08-28T12:39:06+03:00"
  }
}
```

Features are ordered most-urgent first, and ties are broken by whichever has
been in that state longest — so the thing you have ignored longest is always
at the top. `summary.status` is the single most urgent status across
everything, which is what a one-colour indicator should follow.

`tokens` and `cost` cover the whole conversation **including its subagents**.
The Task tool gives every subagent its own session in the same directory, so
one conversation routinely looks like four; those sessions are folded into
the parent's totals rather than counted separately, because their spend is
real spend on this feature. `worked_seconds` deliberately does not include
them: a parent's turn stays open while a subagent runs, so its duration
already covers that work.

`todos` is the agent's own task list — the one opencode and Claude Code
maintain while working through a multi-step job. It is the only genuine
progress signal either tool exposes (there is no percentage or ETA anywhere
in their APIs), and `current` is usually the most informative single line you
can show: what the agent is actually doing right now. Snapshots are emitted
when todo progress moves even if the headline status has not changed, so a
progress gauge keeps up while an agent stays `working`.

`git` is where the feature's branch stands relative to the project's default
branch: `ahead` and `behind` in commits, `insertions` and `deletions` from the
diff against the base's current tip, and `uncommitted` as a count of files with
changes that are not committed at all — the only field that reflects the
working tree rather than committed history.

It is measured on its own slow ticker (`--git`, 30 seconds by default) instead
of during a snapshot rebuild. A rebuild is cheap, reads a few small files and
runs on a 150ms debounce; worktree status is not, and spawns several git
subprocesses per feature. Computing it inline would put dozens of git processes
a second into a daemon that otherwise does no repository I/O at all, so expect
`git` to lag a commit by up to that interval. The object is absent entirely
until the first measurement lands, which is deliberately distinct from a
measured all-zero: "nothing committed yet" is a real answer worth showing.

`since` is when a feature entered its current status, and is deliberately
**not** refreshed while the status is unchanged, so a "how long has it been
working / idle" gauge can tick locally without the stream having to emit on a
timer. A consumer should render elapsed time itself from `since`.

It is driven by opencode's event stream rather than polling, so it costs
nothing while nothing is happening. Because which event means what has varied
between opencode versions — and whether permissions are ever requested at all
depends on your own permission config — an event is treated only as
"something changed, go look"; the authoritative state always comes from
re-reading the HTTP API, with a slow safety-net refresh (`--safety`, default
30s) so a missed event can leave the view briefly stale but never
permanently wrong.

## On disk

```
~/.local/state/canaveral/
  projects.json                       the project registry: name -> checkout, last used
  features/<project>/<feature>.json   slot, branch, ports, units, windows
  logs/<project>/<feature>/*.log      service and agent logs
  worktrees/<project>/<feature>/      the feature checkout, only with [worktree] root = "state"
```

By default a feature's checkout lives beside its project instead, at
`<project_root>/worktrees/<feature>/` (see `[worktree] root` above).

Removing a feature closes only the windows canaveral opened — anything you
opened on that workspace yourself is moved to an ordinary workspace on the
same monitor rather than closed, since it may hold real work. That also lets
the feature's workspace be released instead of lingering with your window
stranded on it.

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
creation. `--focus` (and `canaveral-goto` / clicking a bar's slot) explicitly
pulls the workspace back onto whichever monitor you're currently on before
switching to it. On a single-monitor machine there's nowhere to hide the work,
so it briefly flashes your current workspace and restores it afterwards
instead.

## Status bar integration

A bar reads `canaveral watch` (see above) for feature state, and `canaveral
ws-slot` for the number each feature answers to.

Slot numbers are stable. A feature is assigned the lowest free number when it is
created and keeps it until it is removed, so `super+ctrl+2` means the same
workspace tomorrow; removing a feature frees its number for the next one, which
keeps the list dense enough for a fixed row of widgets. The numbering is global
across projects, unlike the per-project slot that derives ports, since a bar
shows every project at once. `watch` emits it as `ws_slot` per feature, so a bar
can label and order cards by the same number the jump keybinds use.

```
$ canaveral ws-slot
SLOT  WORKSPACE                 FEATURE
1     norules:small-fixes       norules/small-fixes
2     canaveral:install-script  canaveral/install-script

$ canaveral ws-slot 2
canaveral:install-script
```

Bind those numbers to jumps, so slot N is one keystroke away:

```
bind = $mainMod CTRL, 1, exec, ~/.config/hypr/bin/canaveral-goto 1
# ...through 9
```

where `canaveral-goto N` resolves `canaveral ws-slot N` and dispatches to that
workspace, pulling it onto your current monitor first if canaveral built it
elsewhere. `canaveral ws-slot N --json` prints `{"text": "...", "class":
"active|inactive|hidden"}` for bars that want one widget per slot and style it
by class.

## Remembering a layout you dragged

```
canaveral hyprwatch --install   # writes and enables a systemd --user unit
```

`canaveral hyprwatch` subscribes to Hyprland's event socket and, the moment you
leave a feature's workspace, records the current column ratios into
`[layout.current]` (see "Windows" above). It sits idle at 0% CPU between events.
Without it, a `reset` restores the manifest's `[layout.default]` rather than the
widths you last dragged.

## Requirements

systemd user manager, git, `opencode`, and Hyprland for the window layer. Without
Hyprland the window step is skipped with a warning and everything else works.

## Building and installing

```
scripts/build.sh                  # -> ./bin/canaveral
scripts/install.sh                # build, stop canaveral units, install to ~/.local/bin
scripts/install.sh --dry-run      # show what it would do, change nothing
```

Flags: `--prefix DIR` (or `CANAVERAL_PREFIX`) to install somewhere other than
`~/.local/bin`, `--keep-features` to leave feature units running, `--no-build`
to install the existing `./bin/canaveral`, `--dry-run` to rehearse.

### What a reinstall actually does

Replacing the binary is not just a copy, because running units hold the old
executable open and one unit file hard-codes its path. `install.sh` goes through
these steps in order:

1. **Preflight.** Requires a Go toolchain, on `PATH` or via `mise`. Missing
   `git`, `systemctl`, `opencode`, `hyprctl` or `mise` only warn.
2. **Build** with the version stamped in (see below), and read the new version
   back out of `./bin/canaveral`.
3. **Stop every `canaveral-*` systemd user unit** — feature agents, feature
   services, and `canaveral-hyprwatch.service`. Whether hyprwatch was active is
   remembered for step 6. With `--keep-features`, only hyprwatch is stopped.
4. **Except its own unit.** Installing from inside a canaveral agent is normal,
   and stopping that unit would kill the script mid-copy, so the unit found in
   `/proc/self/cgroup` is skipped and reported.
5. **Install atomically**: copy to a temp file in the target directory and
   `mv` it into place, so nothing ever reads a half-written binary and a
   still-open old inode cannot cause `ETXTBSY`. Then verify — if the installed
   binary does not report the version just built, the script fails here, before
   anything is started again.
6. **Reinstall `canaveral-hyprwatch.service`** if it was running, by invoking
   `canaveral hyprwatch --install` from the newly installed binary. This is a
   reinstall rather than a restart on purpose: the unit's `ExecStart` contains
   the symlink-resolved absolute path of whichever binary installed it, so a
   plain `systemctl restart` would keep running the old build.

### Restarting features

Feature agents and services are **not** brought back automatically. They are
transient units started through `systemd-run`, so once stopped they no longer
exist for `systemctl start` to act on; only canaveral can recreate them with the
right worktree, environment and log paths. The script prints how many it stopped
— restore them per feature with:

```
canaveral reset <feature>
```

Use `--keep-features` to skip the teardown entirely. That leaves those agents
running on the old executable until they are next restarted, which is fine when
the change only touches CLI behaviour.

### Version stamping

The build stamps the git description, commit and build time into the binary, so
you can always tell which source an installed executable came from — useful when
several worktrees can each build one:

```
$ canaveral --version
canaveral 8d9fb0e-dirty built 2026-08-31T07:29:42Z
```

A `-dirty` suffix means it was built from uncommitted changes. Set
`CANAVERAL_VERSION` to override the derived string for a release build.

Between releases the description carries the distance from the last tag —
`v0.1.0-3-gbb9dacf` is three commits past `v0.1.0` — so a running binary maps
back to an entry in [VERSIONS.md](VERSIONS.md).

## Testing

```
go test ./...
go test -race ./...
```
