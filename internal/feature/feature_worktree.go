package feature

// Worktree provisioning: creating the git worktree, copying/linking files
// into it, running [worktree].setup and [database].setup.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/toolchain"
	"github.com/bandito/canaveral/internal/worktree"
)

func ensureWorktree(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, opt Options, created bool, r Reporter) error {

	if !worktree.IsRepo(ctx, m.Root) {
		return fmt.Errorf("%s is not a git repository; canaveral needs one to create feature worktrees", m.Root)
	}
	res, err := worktree.Ensure(ctx, m.Root, f.Worktree, f.Branch, opt.Base)
	if err != nil {
		return err
	}
	if !res.Created {
		if created {
			r.Info("reusing existing worktree %s", f.Worktree)
		}
		return nil
	}
	r.OK("worktree %s on %s", f.Worktree, f.Branch)

	env, err := provisionWorktree(ctx, m, f, vars, r)
	if err != nil {
		return err
	}
	linkNamespaceSkill(f, r)
	return runDatabaseSetup(ctx, m, f, vars, env, r)
}

// provisionWorktree links/copies m's [worktree] files and runs its setup
// command into the just-created worktree, returning the merged environment
// later steps (database setup) also need.
func provisionWorktree(ctx context.Context, m *manifest.Manifest, f *state.Feature, vars tmpl.Vars, r Reporter) (map[string]string, error) {
	tc, err := toolchain.Env(ctx, m.ToolchainMode(), f.Worktree)
	if err != nil {
		return nil, err
	}
	env := baseEnvFor(m, f, tc)

	setup, err := tmpl.Render("worktree.setup", m.Worktree.Setup, vars)
	if err != nil {
		return nil, err
	}
	f.Provisioned = append(append([]string{}, m.Worktree.Copy...), manifest.FileName)
	prov := worktree.Provision{
		Link: m.Worktree.Link,
		// The manifest is copied so the worktree is self-describing even when
		// canaveral.toml is untracked and therefore absent from the checkout.
		Copy:         f.Provisioned,
		Setup:        setup,
		SetupTimeout: m.Worktree.SetupTimeout.Duration,
		Env:          manifest.MergeEnv(env, m.Env),
	}
	if err := prov.Apply(ctx, m.Root, f.Worktree, r.Info); err != nil {
		return nil, err
	}
	return env, nil
}

// linkNamespaceSkill links f's namespace's shared skill into its worktree,
// if it has a namespace. Best-effort: a namespace skill is a convenience,
// and failing to link one must not fail the whole worktree creation.
func linkNamespaceSkill(f *state.Feature, r Reporter) {
	ns := Namespace(f.Name)
	if ns == "" {
		return
	}
	rel, linked, err := skills.Link(f.Worktree, f.Project, ns)
	if err != nil {
		r.Warn("namespace skill: %v", err)
		return
	}
	f.Provisioned = append(f.Provisioned, rel)
	if linked {
		r.OK("linked namespace skill %s", ns)
	}
}

// runDatabaseSetup runs [database].setup, if declared. Runs after
// provisioning so .env and credentials already exist.
func runDatabaseSetup(ctx context.Context, m *manifest.Manifest, f *state.Feature, vars tmpl.Vars, env map[string]string, r Reporter) error {
	dbSetup, err := tmpl.Render("database.setup", m.Database.Setup, vars)
	if err != nil {
		return err
	}
	if strings.TrimSpace(dbSetup) == "" {
		return nil
	}
	if f.DBSuffix != "" {
		r.Info("preparing databases (suffix %s)", f.DBSuffix)
	} else {
		r.Info("preparing databases")
	}
	return worktree.RunHook(ctx, "database setup", f.Worktree, dbSetup,
		manifest.MergeEnv(env, m.Env), m.Database.SetupTimeout.Or(defaultSetupTimeout))
}

// defaultSetupTimeout bounds a create-time setup command that has no explicit
// one. Generous, because a first provision legitimately installs dependencies.
const defaultSetupTimeout = 10 * time.Minute

// defaultPrecheckTimeout bounds a precheck with no explicit one. Much shorter
// than setup: a precheck runs on every open and asserts things that are either
// already true or quick to make true, so a long wait means something is wrong
// rather than that work is being done.
const defaultPrecheckTimeout = 5 * time.Minute

// runPrecheck satisfies the project's declared preconditions before anything
// starts.
//
// Runs on every open, unlike worktree and database setup, which provision a
// worktree once when it is created. The distinction is what the command is
// asserting: setup is about the worktree, which stays as it was left, while a
// precheck is about the machine around it, which does not. A database server
// stops, a colleague merges a migration — nothing touched the worktree, and
// the feature no longer comes up.
func runPrecheck(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, tc map[string]string, r Reporter) error {

	cmd, err := tmpl.Render("precheck", m.Precheck, vars)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cmd) == "" {
		return nil
	}

	r.Step("precheck  %s", cmd)
	// The same env services get, so a precheck can run the project's own
	// tooling: the toolchain PATH is what makes `bin/rails` resolve at all,
	// and the port and database-suffix variables are what make it act on this
	// feature's resources rather than another's.
	env := manifest.MergeEnv(baseEnvFor(m, f, tc), m.Env)
	if err := worktree.RunHook(ctx, "precheck", f.Worktree, cmd, env,
		m.PrecheckTimeout.Or(defaultPrecheckTimeout)); err != nil {
		return err
	}
	r.OK("precheck")
	return nil
}
