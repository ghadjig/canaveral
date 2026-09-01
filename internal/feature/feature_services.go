package feature

// Service reconciliation: starting/adopting [[services]] units and
// restarting them on demand.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/probe"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/toolchain"
	"github.com/bandito/canaveral/internal/unit"
)

func reconcileServices(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, tc map[string]string, res *Result, r Reporter, prog *progress) error {

	base := baseEnvFor(m, f, tc)
	var records []state.Service

	logDir, err := state.LogDir(f.Project, f.Name)
	if err != nil {
		return err
	}

	for _, s := range m.Services {
		prog.start("service " + s.Name)
		rec := serviceRecord(m, f, s, logDir)
		cmd, err := tmpl.Render("service."+s.Name+".cmd", s.Cmd, vars)
		if err != nil {
			return err
		}
		rec.Cmd = cmd

		if st, err := unit.Query(ctx, rec.Unit); err == nil && st.Running() {
			records = append(records, rec)
			prog.done()
			continue
		}

		// Recorded before the start request, not after: systemd owns the unit
		// from the moment the request lands, so anything that interrupts the
		// call in between must still be able to find it again.
		res.launched = append(res.launched, rec.Unit)

		started, err := startService(ctx, m, f, s, rec, base, vars, r)
		if err != nil {
			return err
		}
		if !started {
			// Optional service that failed; not recorded as running.
			prog.done()
			continue
		}
		res.StartedSvc = append(res.StartedSvc, s.Name)
		records = append(records, rec)

		// Persist per service rather than once at the end. A later service
		// failing used to discard the record of every healthy one before it,
		// which left `rm` with nothing to stop and the ports held forever.
		f.Services = records
		if err := state.Save(f); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
		prog.done()
	}
	f.Services = records
	return nil
}

// serviceRecord derives the bookkeeping for one service. Cmd is left to the
// caller, which is the only part that can fail to render.
func serviceRecord(m *manifest.Manifest, f *state.Feature, s manifest.Service, logDir string) state.Service {
	return state.Service{
		Name:     s.Name,
		Unit:     unit.Name(f.Project+"-"+f.Name, "svc", s.Name),
		Dir:      serviceDir(f, m, s.Dir),
		LogPath:  filepath.Join(logDir, "svc-"+s.Name+".log"),
		Optional: s.Optional,
	}
}

// startService launches one service and waits for its ready probe.
//
// Reports false, nil when an optional service failed: the caller must not
// record it as running, but it is not an error either. A required service's
// failure is returned.
func startService(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	s manifest.Service, rec state.Service, base map[string]string,
	vars tmpl.Vars, r Reporter) (bool, error) {

	svcEnv, err := tmpl.RenderMap("service."+s.Name+".env", s.Env, vars)
	if err != nil {
		return false, err
	}
	ready, err := renderReady(s.Name, s.Ready, vars)
	if err != nil {
		return false, err
	}

	unit.Reset(ctx, rec.Unit)
	r.Step("service %s  %s", s.Name, rec.Cmd)
	if err := unit.Start(ctx, unit.Spec{
		Name:        rec.Unit,
		Description: fmt.Sprintf("canaveral %s/%s service %s", f.Project, f.Name, s.Name),
		Dir:         rec.Dir,
		Cmd:         rec.Cmd,
		Env:         manifest.MergeEnv(base, m.Env, svcEnv),
		LogPath:     rec.LogPath,
	}); err != nil {
		if s.Optional {
			r.Warn("optional service %s: %v", s.Name, err)
			return false, nil
		}
		return false, err
	}

	if k := ready.Kind(); k != "" {
		// Say so before blocking. ready.timeout is routinely a minute or two
		// for anything as slow to boot as a Rails server, and a terminal that
		// goes silent for that long is indistinguishable from a hang.
		r.Info("waiting for %s readiness probe, up to %s", k, ready.Timeout.Or(probe.DefaultTimeout))
		if err := probe.Wait(ctx, ready, rec.Dir, rec.LogPath, aliveCheck(ctx, rec.Unit, rec.LogPath)); err != nil {
			_ = unit.Stop(ctx, rec.Unit)
			unit.Reset(ctx, rec.Unit)
			// A dead process already reports its log through aliveCheck. A
			// timeout is the other case: still running, still not ready, and
			// its own output is then the only thing that says why — a Rails
			// server answering 500 because the database is down looks
			// identical from outside to one that is merely slow.
			if errors.Is(err, probe.ErrTimeout) {
				err = fmt.Errorf("%w\n%s", err, tailIndent(rec.LogPath, 15))
			}
			if s.Optional {
				r.Warn("optional service %s: %v", s.Name, err)
				return false, nil
			}
			return false, fmt.Errorf("service %q: %w", s.Name, err)
		}
		r.OK("service %s ready", s.Name)
	} else {
		r.OK("service %s started", s.Name)
	}
	return true, nil
}

func renderReady(name string, ready manifest.Ready, vars tmpl.Vars) (manifest.Ready, error) {
	var err error
	if ready.HTTP, err = tmpl.Render("service."+name+".ready.http", ready.HTTP, vars); err != nil {
		return ready, err
	}
	if ready.TCP, err = tmpl.Render("service."+name+".ready.tcp", ready.TCP, vars); err != nil {
		return ready, err
	}
	if ready.Cmd, err = tmpl.Render("service."+name+".ready.cmd", ready.Cmd, vars); err != nil {
		return ready, err
	}
	return ready, nil
}

// RestartServices stops and restarts the named services of a feature, waiting
// on each one's ready probe before moving to the next.
//
// Deliberately not a `systemctl restart`: that would reuse the old log file and
// return the moment systemd had forked, so a caller could not tell a service
// that came back up from one that died on start. Going through the same path as
// the initial start truncates the log and honours the manifest's `ready`, which
// is the whole reason to prefer this over restarting the unit by hand.
//
// Only services named in the manifest can be restarted; an unknown name is an
// error listing what is available, rather than a silent no-op.
func RestartServices(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	names []string, r Reporter) error {

	if len(names) == 0 {
		return fmt.Errorf("name at least one service to restart")
	}

	byName := make(map[string]manifest.Service, len(m.Services))
	for _, s := range m.Services {
		byName[s.Name] = s
	}
	wanted := make([]manifest.Service, 0, len(names))
	for _, n := range names {
		s, ok := byName[n]
		if !ok {
			return fmt.Errorf("no service %q in %s; %s has: %s",
				n, manifest.FileName, f.Key(), strings.Join(serviceNames(m), ", "))
		}
		wanted = append(wanted, s)
	}

	tc, err := toolchain.Env(ctx, m.ToolchainMode(), f.Worktree)
	if err != nil {
		return err
	}
	base := baseEnvFor(m, f, tc)
	vars := varsFor(ctx, m, f, false)

	logDir, err := state.LogDir(f.Project, f.Name)
	if err != nil {
		return err
	}

	for _, s := range wanted {
		rec := serviceRecord(m, f, s, logDir)
		cmd, err := tmpl.Render("service."+s.Name+".cmd", s.Cmd, vars)
		if err != nil {
			return err
		}
		rec.Cmd = cmd

		// Stop unconditionally: a unit that already exited still holds a
		// failed state that would make the start below refuse.
		if err := unit.Stop(ctx, rec.Unit); err != nil {
			r.Info("stop %s: %v", s.Name, err)
		}
		unit.Reset(ctx, rec.Unit)

		started, err := startService(ctx, m, f, s, rec, base, vars, r)
		if err != nil {
			return err
		}
		f.Services = upsertService(f.Services, rec, started)
	}
	return state.Save(f)
}

// serviceNames lists the manifest's services for an error message, saying so
// explicitly when there are none rather than trailing off after a colon.
func serviceNames(m *manifest.Manifest) []string {
	if out := m.ServiceNames(); len(out) > 0 {
		return out
	}
	return []string{"(none declared)"}
}

// upsertService replaces a service's record, or drops it when an optional
// service failed to come back, so state keeps matching what is running.
func upsertService(list []state.Service, rec state.Service, running bool) []state.Service {
	out := make([]state.Service, 0, len(list)+1)
	for _, s := range list {
		if s.Name != rec.Name {
			out = append(out, s)
		}
	}
	if running {
		out = append(out, rec)
	}
	return out
}
