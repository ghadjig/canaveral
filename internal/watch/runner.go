package watch

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/ocevents"
	"github.com/bandito/canaveral/internal/state"
)

// Options tunes the runner's timings.
type Options struct {
	// Project limits the watch to one project; empty watches every project.
	Project string
	// Debounce coalesces the burst of events a single turn produces (a
	// prompt emits several session.status and message.* events within
	// milliseconds) into one refresh.
	Debounce time.Duration
	// Rescan is how often the feature list is re-read from disk, picking up
	// features created or removed since. Cheap: it only reads small JSON
	// files.
	Rescan time.Duration
	// Safety is a full refresh interval that runs regardless of events, so
	// a missed or unrecognised event can leave the view briefly stale but
	// never permanently wrong.
	Safety time.Duration
	// Git is how often each feature's branch status is remeasured. Far
	// slower than the rest deliberately: it costs several git subprocesses
	// per feature, and commit counts do not move on the timescale the other
	// fields do. See gitCache.
	Git time.Duration
}

func (o Options) withDefaults() Options {
	if o.Debounce <= 0 {
		o.Debounce = 150 * time.Millisecond
	}
	if o.Rescan <= 0 {
		o.Rescan = 3 * time.Second
	}
	if o.Safety <= 0 {
		o.Safety = 30 * time.Second
	}
	if o.Git <= 0 {
		o.Git = 30 * time.Second
	}
	return o
}

// Runner keeps the live view current and emits a snapshot on every change.
type Runner struct {
	opt Options

	mu   sync.Mutex
	prev map[string]Feature // feature key -> last built view
	subs map[string]context.CancelFunc

	// health caches the last full refresh's probe results, keyed by feature and
	// then agent name, so the fast lifecycle poll can rebuild a snapshot
	// without issuing HTTP of its own.
	health map[string]map[string]agent.Health
	// inPhase is the set of feature keys that were mid-lifecycle at the last
	// poll, so leaving a phase can be noticed and acted on.
	inPhase map[string]bool

	trigger chan struct{}
	// git is measured out of band; see gitCache.
	git *gitCache
	// probe is swappable so tests do not need real agent servers.
	probe func(ctx context.Context, url, dir string) agent.Health
	// load is swappable for the same reason.
	load func(project string) ([]*state.Feature, error)
	now  func() time.Time
}

// NewRunner builds a runner with the default, real data sources.
func NewRunner(opt Options) *Runner {
	return &Runner{
		opt:     opt.withDefaults(),
		prev:    map[string]Feature{},
		subs:    map[string]context.CancelFunc{},
		trigger: make(chan struct{}, 1),
		git:     newGitCache(),
		probe:   agent.Probe,
		load:    loadFeatures,
		now:     time.Now,
	}
}

func loadFeatures(project string) ([]*state.Feature, error) {
	// Widget slots are global, so allocate across every project even when
	// only one is being watched: the number a bar renders has to match the
	// one the jump keybinds resolve, and those do not know about projects.
	all, err := state.EnsureWSlots()
	if err != nil {
		return nil, err
	}
	if project == "" {
		return all, nil
	}
	var out []*state.Feature
	for _, f := range all {
		if f.Project == project {
			out = append(out, f)
		}
	}
	return out, nil
}

// Run streams a JSON snapshot per line to w until ctx is cancelled.
//
// A snapshot is written on any change, plus once at startup so a consumer
// that has just launched immediately has something to render rather than a
// blank widget until the next event.
func (r *Runner) Run(ctx context.Context, w io.Writer) error {
	enc := json.NewEncoder(w)

	// Every snapshot is flushed as it is produced. A buffered writer would
	// otherwise hold the startup snapshot until enough later output
	// accumulated to fill it, which for a quiet set of features could be
	// indefinitely — the consumer would sit blank despite having valid
	// state waiting a few hundred bytes away.
	write := func(s Snapshot) {
		// A failed write (the consumer went away) is not fatal; it may be
		// restarted and reconnect to a fresh process.
		_ = enc.Encode(s)
		if f, ok := w.(interface{ Flush() error }); ok {
			_ = f.Flush()
		}
	}

	r.resubscribe(ctx)
	go r.runGit(ctx)
	snap, _ := r.refresh(ctx)
	write(snap)

	emit := func() {
		if s, changed := r.refresh(ctx); changed {
			write(s)
		}
	}

	rescan := time.NewTicker(r.opt.Rescan)
	defer rescan.Stop()
	safety := time.NewTicker(r.opt.Safety)
	defer safety.Stop()

	// A feature being created or torn down publishes its progress into its own
	// state file, and nothing pushes an event when it does: the process doing
	// the work is a different one, with no channel back here. Seeing it move
	// means looking, and looking on the ordinary rescan is far too slow — at a
	// three-second interval the whole of a short creation can pass between two
	// ticks, which is exactly what happened the first time this was tested.
	//
	// So state is re-read on its own fast ticker, reusing the agent health probes
	// from the last full refresh rather than issuing new ones. That is what
	// makes 200ms affordable: the expensive half of a refresh is the HTTP, and
	// progress does not come from HTTP. Reading a handful of small JSON files
	// five times a second costs nothing, and snapshots are still emitted only
	// on change, so a settled world stays silent.
	phase := time.NewTicker(phaseRescan)
	defer phase.Stop()

	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.trigger:
			// Collapse a burst into a single refresh.
			debounce = time.After(r.opt.Debounce)
		case <-debounce:
			debounce = nil
			emit()
		case <-phase.C:
			if s, changed := r.refreshCached(); changed {
				write(s)
			}
		case <-rescan.C:
			r.resubscribe(ctx)
			emit()
		case <-safety.C:
			emit()
		}
	}
}

// phaseRescan is how often state is re-read for lifecycle progress. Fast
// enough that a progress bar moves rather than jumps, cheap enough to leave
// running always because it issues no network calls of its own.
const phaseRescan = 200 * time.Millisecond

// resubscribe starts an SSE watcher for every agent that does not have one
// and stops watchers for agents that have gone away.
func (r *Runner) resubscribe(ctx context.Context) {
	features, err := r.load(r.opt.Project)
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, f := range features {
		for _, a := range f.Agents {
			if a.URL == "" {
				continue
			}
			key := f.Key() + "/" + a.Name
			live[key] = true

			r.mu.Lock()
			_, watching := r.subs[key]
			r.mu.Unlock()
			if watching {
				continue
			}

			sctx, cancel := context.WithCancel(ctx)
			r.mu.Lock()
			r.subs[key] = cancel
			r.mu.Unlock()

			url := a.URL
			go func() {
				_ = ocevents.Watch(sctx, url, func(ev ocevents.Event) {
					if relevant(ev.Type) {
						r.wake()
					}
				})
			}()
		}
	}

	r.mu.Lock()
	for key, cancel := range r.subs {
		if !live[key] {
			cancel()
			delete(r.subs, key)
		}
	}
	r.mu.Unlock()
}

// relevant filters the event firehose down to the ones that can change a
// feature's headline state.
//
// opencode emits a great deal per turn (token deltas, individual message
// parts); reacting to all of it would mean re-probing every agent dozens of
// times a second for no visible difference. The prefixes here are
// deliberately broad because an event only triggers a re-read — being
// slightly over-inclusive costs one HTTP round trip, while being
// under-inclusive would mean missing a state change entirely.
func relevant(t string) bool {
	switch t {
	case "session.idle", "session.error", "session.status", "session.created", "session.deleted",
		"permission.asked", "permission.replied",
		"permission.v2.asked", "permission.v2.replied",
		"question.asked", "question.replied", "question.rejected",
		"question.v2.asked", "question.v2.replied", "question.v2.rejected",
		"todo.updated",
		"session.next.tool.called", "session.next.tool.success", "session.next.tool.failed",
		"server.connected":
		return true
	}
	return false
}

func (r *Runner) wake() {
	select {
	case r.trigger <- struct{}{}:
	default: // a refresh is already pending
	}
}

// refresh rebuilds the whole view and reports whether it differs from the
// last one, so identical snapshots are not re-emitted.
func (r *Runner) refresh(ctx context.Context) (Snapshot, bool) {
	features, err := r.load(r.opt.Project)
	if err != nil {
		return Snapshot{Time: r.now()}, false
	}

	now := r.now()

	// Probe agents concurrently: serial HTTP with timeouts would make the
	// refresh latency scale with the number of features.
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = map[string]map[string]agent.Health{}
	)
	for _, f := range features {
		for _, a := range f.Agents {
			if a.URL == "" {
				continue
			}
			f, a := f, a
			wg.Add(1)
			go func() {
				defer wg.Done()
				h := r.probe(ctx, a.URL, a.Dir)
				mu.Lock()
				if results[f.Key()] == nil {
					results[f.Key()] = map[string]agent.Health{}
				}
				results[f.Key()][a.Name] = h
				mu.Unlock()
			}()
		}
	}
	wg.Wait()

	r.mu.Lock()
	r.health = results
	r.mu.Unlock()

	return r.rebuild(features, results, now)
}

// refreshCached rebuilds from state alone, reusing the probe results of the
// last full refresh.
//
// This is the lifecycle-progress path. Progress is written to state by another
// process entirely and owes nothing to the agent API, so re-reading a few small
// JSON files answers it completely — and skipping the HTTP is what makes it
// cheap enough to run five times a second, forever.
func (r *Runner) refreshCached() (Snapshot, bool) {
	features, err := r.load(r.opt.Project)
	if err != nil {
		return Snapshot{Time: r.now()}, false
	}
	r.mu.Lock()
	health := r.health
	was := r.inPhase
	now := map[string]bool{}
	for _, f := range features {
		if f.InPhase() {
			now[f.Key()] = true
		}
	}
	r.inPhase = now
	r.mu.Unlock()

	// The instant a feature stops booting is the instant the cached probes
	// become wrong about it: they were taken while its agent did not exist yet,
	// so reusing them would report a freshly created feature as offline. Ask
	// for a real refresh and publish nothing in the meantime — emitting the
	// stale reading first would flash "offline" between "booting" and "idle",
	// which is precisely the moment someone is watching the row.
	for k := range was {
		if !now[k] {
			r.wake()
			snap, _ := r.rebuild(features, health, r.now())
			return snap, false
		}
	}
	return r.rebuild(features, health, r.now())
}

// rebuild assembles the snapshot and reports whether it differs from the last.
func (r *Runner) rebuild(features []*state.Feature,
	results map[string]map[string]agent.Health, now time.Time) (Snapshot, bool) {

	built := make([]Feature, 0, len(features))

	r.mu.Lock()
	next := make(map[string]Feature, len(features))
	for _, f := range features {
		var prev *Feature
		if p, ok := r.prev[f.Key()]; ok {
			prev = &p
		}
		v := Build(f, results[f.Key()], prev, now)
		// Measured on its own schedule, so it is attached here rather than
		// computed inside Build.
		v.Git = r.git.get(f.Key())
		next[f.Key()] = v
		built = append(built, v)
	}
	changed := !sameView(r.prev, next)
	r.prev = next
	r.mu.Unlock()

	return Summarise(built, now), changed
}

// sameView compares two views ignoring fields that change on every rebuild
// but carry no new information, so an unchanged world does not produce a
// stream of identical snapshots.
func sameView(a, b map[string]Feature) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameFeature(av, bv) {
			return false
		}
	}
	return true
}

// sameActivity compares the in-flight tool call, so moving from one command
// to the next produces a snapshot even while the status stays "working".
func sameActivity(a, b *agent.Activity) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.Tool == b.Tool && a.Title == b.Title && a.Since.Equal(b.Since)
}

// sameProgress compares lifecycle progress, so a step advancing within an
// unchanged phase still produces a snapshot.
func sameProgress(a, b *Progress) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.Step == b.Step && a.Total == b.Total && a.Label == b.Label
}

// sameTodos compares task lists, so progress within an unchanged status
// (still "working", but now 6 of 9 rather than 5) still produces a snapshot.
func sameTodos(a, b *Todos) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.Total == b.Total && a.Completed == b.Completed &&
		a.InProgress == b.InProgress && a.Cancelled == b.Cancelled &&
		a.Current == b.Current
}

func sameFeature(a, b Feature) bool {
	if a.Status != b.Status || !a.Since.Equal(b.Since) || len(a.Agents) != len(b.Agents) {
		return false
	}
	// Without this a commit would never reach the consumer: the git refresh
	// wakes the runner, but the rebuild would compare equal and be dropped.
	if !sameGit(a.Git, b.Git) {
		return false
	}
	// Progress moves while the status stays "booting", and it is the entire
	// point of that status: without this a progress bar would jump straight
	// from empty to gone.
	if !sameProgress(a.Progress, b.Progress) {
		return false
	}
	for i := range a.Agents {
		x, y := a.Agents[i], b.Agents[i]
		if x.Name != y.Name || x.Status != y.Status || x.Error != y.Error ||
			x.Tokens != y.Tokens || x.Cost != y.Cost || x.Model != y.Model ||
			x.Variant != y.Variant || x.SubAgents != y.SubAgents ||
			x.LastUser != y.LastUser || x.LastAssistant != y.LastAssistant {
			return false
		}
		if !sameTodos(x.Todos, y.Todos) {
			return false
		}
		if !sameActivity(x.Activity, y.Activity) {
			return false
		}
		if (x.Pending == nil) != (y.Pending == nil) {
			return false
		}
		if x.Pending != nil && (x.Pending.Kind != y.Pending.Kind || x.Pending.Header != y.Pending.Header) {
			return false
		}
	}
	return true
}
