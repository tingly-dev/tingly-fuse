// Package manager ties repositories, sessions and backends together. It is
// the API the CLI (and later the daemon and MCP server) call into.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
	"github.com/tingly-dev/tingly-fuse/internal/session"
)

// Environment variables set for every process run inside a view, so tools
// and scripts can avoid hard-coding the workspace path.
const (
	EnvWorkspace = "TINGLY_WORKSPACE"
	EnvSession   = "TINGLY_SESSION"
	EnvRepo      = "TINGLY_REPO"
)

// DefaultHome is $TINGLY_FUSE_HOME, or ~/.tingly-fuse.
func DefaultHome() (string, error) {
	if h := os.Getenv("TINGLY_FUSE_HOME"); h != "" {
		return h, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".tingly-fuse"), nil
}

type Manager struct {
	Home     string
	sessions *session.Store
}

func New(home string) *Manager {
	return &Manager{Home: home, sessions: session.NewStore(home)}
}

// AddDir snapshots directory dir as the base new sessions of dir
// repository name start from, registering name first if needed (ADR 0013).
// It returns the repository and the new base's id.
func (m *Manager) AddDir(ctx context.Context, name, dir string, opts repo.DirOptions) (*repo.Repo, string, error) {
	return repo.AddDir(ctx, m.Home, name, dir, opts)
}

// RemoveRepo unregisters a repository and deletes its mirror and bases.
// It refuses while sessions still use the repository unless force is set,
// in which case those sessions are dropped first (unpushed work is lost).
func (m *Manager) RemoveRepo(ctx context.Context, name string, force bool) error {
	if _, err := repo.Open(m.Home, name); err != nil {
		return err
	}
	sessions, err := m.sessions.List()
	if err != nil {
		return err
	}
	var ids []string
	for _, s := range sessions {
		if s.Repo == name {
			ids = append(ids, s.ID)
		}
	}
	if len(ids) > 0 && !force {
		return fmt.Errorf("repo %q still has sessions %v; drop them or use --force", name, ids)
	}
	for _, id := range ids {
		if err := m.Drop(ctx, id); err != nil {
			return fmt.Errorf("drop session %s: %w", id, err)
		}
	}
	return repo.Remove(m.Home, name)
}

// Repos lists registered repositories.
func (m *Manager) Repos() ([]*repo.Repo, error) { return repo.List(m.Home) }

// Repo loads one registered repository.
func (m *Manager) Repo(name string) (*repo.Repo, error) { return repo.Open(m.Home, name) }

// SpawnOptions say where a session of a dir repository starts and what it
// shares. Git repositories spawn with Git().Spawn.
type SpawnOptions struct {
	Repo    string
	Base    string // base id; the repository's latest snapshot when empty
	ID      string // optional; random when empty
	Backend string // optional; backend.Default when empty
	// Share adds shared paths for this session on top of the repository's
	// (see SetShares). It shares them with every other session that shares
	// the same path, through the repository's one copy of it.
	Share []string
}

// Spawn creates a session of a dir repository: a private view of one of its
// snapshots.
func (m *Manager) Spawn(ctx context.Context, opts SpawnOptions) (*session.Session, error) {
	r, err := repo.Open(m.Home, opts.Repo)
	if err != nil {
		return nil, err
	}
	if r.IsGit() {
		return nil, fmt.Errorf("repo %q is a git repository; spawn it with git worktree add", r.Name)
	}
	b, err := backend.Get(orDefault(opts.Backend, backend.Default))
	if err != nil {
		return nil, err
	}
	// Keep the chosen base from being removed until the session is saved.
	unlock, err := r.LockBases(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Re-read: a base add may have made another snapshot current, and a
	// prune removed the one read above, before the lock was ours.
	if r, err = repo.Open(m.Home, opts.Repo); err != nil {
		return nil, err
	}
	id := orDefault(opts.Base, r.Current)
	baseDir, err := existingBase(r, id)
	if err != nil {
		return nil, err
	}
	return m.newSession(ctx, r, b, baseDir, &session.Session{ID: opts.ID, Base: id, Source: r.Source}, opts.Share, nil)
}

// existingBase is the directory of r's base id, which must exist.
func existingBase(r *repo.Repo, id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("invalid base id %q", id)
	}
	dir := r.BaseDir(id)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("repo %q has no base %q (see base list %s)", r.Name, id, r.Name)
	}
	return dir, nil
}

// newSession creates sess's view of baseDir with its shared paths, runs
// setup in it, and saves sess. sess comes with its base filled in, and its
// id when the caller chose one. On error nothing of the session is left.
func (m *Manager) newSession(ctx context.Context, r *repo.Repo, b backend.Backend, baseDir string, sess *session.Session, share []string, setup func(ws backend.Workspace, sess *session.Session, shares []repo.Share) error) (_ *session.Session, err error) {
	shared, err := repo.MergeSharePaths(r.SharedPaths, share)
	if err != nil {
		return nil, err
	}
	if sess.ID == "" {
		sess.ID = session.NewID()
	}
	if err := m.sessions.Reserve(sess.ID); err != nil {
		return nil, err
	}
	ws := backend.Workspace{ID: sess.ID, Repo: r.Name, BaseDir: baseDir, StateDir: m.sessions.Dir(sess.ID)}
	defer func() {
		if err != nil {
			_ = b.Remove(context.WithoutCancel(ctx), ws)
			if r.SharedGit {
				_ = r.RemoveSessionGitDir(sess.ID)
			}
			_ = m.sessions.Delete(sess.ID)
		}
	}()

	// Seeding a share is permanent, so it waits until the id is ours.
	shares, err := r.EnsureShares(ctx, baseDir, shared)
	if err != nil {
		return nil, err
	}
	ws.Shares = toBackendShares(shares)
	if ws.ViewPath, err = b.Create(ctx, ws); err != nil {
		return nil, fmt.Errorf("create view: %w", err)
	}
	sess.Repo, sess.Backend, sess.ViewPath, sess.Shared = r.Name, b.Name(), ws.ViewPath, shared
	sess.CreatedAt = time.Now().UTC()
	if setup != nil {
		if err = setup(ws, sess, shares); err != nil {
			return nil, err
		}
	}
	if err = m.sessions.Save(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// BaseInfo describes one base of a repository.
type BaseInfo struct {
	ID       string
	Head     string // git: commit the base is at
	Current  bool   // dir: the base new sessions start from
	Created  time.Time
	Sessions []string // ids of the sessions layered on it
	Pinned   bool     // its objects are protected from gc in the mirrors
}

// Bases lists the repository's bases with the sessions that use them.
func (m *Manager) Bases(ctx context.Context, name string) ([]BaseInfo, error) {
	r, err := repo.Open(m.Home, name)
	if err != nil {
		return nil, err
	}
	return m.bases(ctx, r, false)
}

// bases lists r's bases. With strict set it fails when a session's metadata
// is unreadable, since that session may use any of them.
func (m *Manager) bases(ctx context.Context, r *repo.Repo, strict bool) ([]BaseInfo, error) {
	ids, err := r.Bases()
	if err != nil {
		return nil, err
	}
	sessions, unreadable, err := m.sessions.ListStrict()
	if err != nil {
		return nil, err
	}
	if strict && len(unreadable) > 0 {
		return nil, fmt.Errorf("cannot tell which bases sessions %v use: their session.json is unreadable; fix or drop them first", unreadable)
	}
	pinned, err := r.PinnedBases(ctx)
	if err != nil {
		return nil, err
	}
	var out []BaseInfo
	for _, id := range ids {
		b := BaseInfo{ID: id}
		if fi, err := os.Stat(r.BaseDir(id)); err == nil {
			b.Created = fi.ModTime()
		}
		if r.IsGit() {
			b.Head, _ = r.ReadHead(ctx, id)
		}
		b.Current = id == r.Current
		for _, s := range sessions {
			if s.Repo == r.Name && s.Base == id {
				b.Sessions = append(b.Sessions, s.ID)
			}
		}
		b.Pinned = pinned[id]
		out = append(out, b)
	}
	return out, nil
}

// RemoveBase deletes one base, named by its id or a unique prefix of it.
// It refuses while sessions use the base.
func (m *Manager) RemoveBase(ctx context.Context, name, id string) error {
	r, err := repo.Open(m.Home, name)
	if err != nil {
		return err
	}
	unlock, err := r.LockBases(true)
	if err != nil {
		return err
	}
	defer unlock()
	// Re-read: a base add may have made another snapshot current meanwhile.
	if r, err = repo.Open(m.Home, name); err != nil {
		return err
	}
	bases, err := m.bases(ctx, r, true)
	if err != nil {
		return err
	}
	var match []BaseInfo
	for _, b := range bases {
		if b.ID == id {
			match = []BaseInfo{b}
			break
		}
		if strings.HasPrefix(b.ID, id) {
			match = append(match, b)
		}
	}
	switch {
	case id == "" || len(match) == 0:
		return fmt.Errorf("repo %q has no base %q (see base list)", name, id)
	case len(match) > 1:
		return fmt.Errorf("base %q is ambiguous in repo %q", id, name)
	case len(match[0].Sessions) > 0:
		return fmt.Errorf("base %s is used by sessions %v; drop them first", match[0].ID, match[0].Sessions)
	case match[0].Current:
		return fmt.Errorf("base %s is the snapshot new sessions of %s start from; take a new one with base add first", match[0].ID, name)
	}
	return r.RemoveBase(ctx, match[0].ID)
}

// PruneBases deletes every base no session uses and returns their ids;
// with dryRun set it only returns them.
func (m *Manager) PruneBases(ctx context.Context, name string, dryRun bool) ([]string, error) {
	r, err := repo.Open(m.Home, name)
	if err != nil {
		return nil, err
	}
	unlock, err := r.LockBases(true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Re-read: a base add may have made another snapshot current meanwhile.
	if r, err = repo.Open(m.Home, name); err != nil {
		return nil, err
	}
	if !dryRun {
		// Finish removals an earlier run was interrupted in.
		if err := r.SweepRemoved(); err != nil {
			return nil, err
		}
	}
	bases, err := m.bases(ctx, r, true)
	if err != nil {
		return nil, err
	}
	var pruned []string
	for _, b := range bases {
		if len(b.Sessions) > 0 || b.Current {
			continue
		}
		if !dryRun {
			if err := r.RemoveBase(ctx, b.ID); err != nil {
				return pruned, err
			}
		}
		pruned = append(pruned, b.ID)
	}
	return pruned, nil
}

// Session loads a session by id.
func (m *Manager) Session(id string) (*session.Session, error) { return m.sessions.Get(id) }

// Sessions lists all sessions.
func (m *Manager) Sessions() ([]*session.Session, error) { return m.sessions.List() }

// Exec runs argv inside the session's view.
func (m *Manager) Exec(ctx context.Context, id string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	sess, b, ws, err := m.load(id)
	if err != nil {
		return err
	}
	return m.run(ctx, b, ws, sess, argv, stdin, stdout, stderr)
}

// Drop removes the session's view and metadata. Unpushed work is lost,
// except with shared git, where the session's branch and commits stay in
// the shared repository.
func (m *Manager) Drop(ctx context.Context, id string) error {
	sess, b, ws, err := m.load(id)
	if err != nil {
		return err
	}
	errs := []error{b.Remove(ctx, ws)}
	if r, rerr := repo.Open(m.Home, sess.Repo); rerr != nil {
		errs = append(errs, rerr)
	} else if r.SharedGit {
		errs = append(errs, r.RemoveSessionGitDir(id))
	}
	return errors.Join(append(errs, m.sessions.Delete(id))...)
}

func (m *Manager) load(id string) (*session.Session, backend.Backend, backend.Workspace, error) {
	sess, err := m.sessions.Get(id)
	if err != nil {
		return nil, nil, backend.Workspace{}, err
	}
	b, err := backend.Get(sess.Backend)
	if err != nil {
		return nil, nil, backend.Workspace{}, err
	}
	r, err := repo.Open(m.Home, sess.Repo)
	if err != nil {
		return nil, nil, backend.Workspace{}, err
	}
	ws := backend.Workspace{
		ID:       sess.ID,
		Repo:     sess.Repo,
		BaseDir:  r.BaseDir(sess.Base),
		StateDir: m.sessions.Dir(sess.ID),
		ViewPath: sess.ViewPath,
	}
	for _, p := range sess.Shared {
		ws.Shares = append(ws.Shares, backend.Share{Path: p, Source: r.ShareSource(p)})
	}
	return sess, b, ws, nil
}

func toBackendShares(shares []repo.Share) []backend.Share {
	var out []backend.Share
	for _, s := range shares {
		out = append(out, backend.Share{Path: s.Path, Source: s.Source, Tracked: s.Tracked})
	}
	return out
}

// Shares returns the paths a repository's new sessions share.
func (m *Manager) Shares(name string) ([]string, error) {
	r, err := repo.Open(m.Home, name)
	if err != nil {
		return nil, err
	}
	return r.SharedPaths, nil
}

// SetShares adds or removes shared paths of a repository; see
// repo.SetShares.
func (m *Manager) SetShares(name string, paths []string, remove bool) (*repo.Repo, error) {
	return repo.SetShares(m.Home, name, paths, remove)
}

func (m *Manager) run(ctx context.Context, b backend.Backend, ws backend.Workspace, sess *session.Session, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd, err := b.Command(ctx, ws, argv)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env,
		EnvWorkspace+"="+ws.ViewPath,
		EnvSession+"="+sess.ID,
		EnvRepo+"="+sess.Repo,
	)
	return cmd.Run()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
