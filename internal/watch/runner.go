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
	return o
}

// Runner keeps the live view current and emits a snapshot on every change.
type Runner struct {
	opt Options

	mu   sync.Mutex
	prev map[string]Feature // feature key -> last built view
	subs map[string]context.CancelFunc

	trigger chan struct{}
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
		probe:   agent.Probe,
		load:    loadFeatures,
		now:     time.Now,
	}
}

func loadFeatures(project string) ([]*state.Feature, error) {
	if project == "" {
		return state.LoadAll()
	}
	return state.LoadProject(project)
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
		case <-rescan.C:
			r.resubscribe(ctx)
			emit()
		case <-safety.C:
			emit()
		}
	}
}

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
	built := make([]Feature, 0, len(features))

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
	next := make(map[string]Feature, len(features))
	for _, f := range features {
		var prev *Feature
		if p, ok := r.prev[f.Key()]; ok {
			prev = &p
		}
		v := Build(f, results[f.Key()], prev, now)
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
