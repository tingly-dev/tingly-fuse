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

// AddRepo registers a repository.
func (m *Manager) AddRepo(ctx context.Context, name, upstream string) (*repo.Repo, error) {
	return m.AddRepoWith(ctx, name, upstream, repo.AddOptions{})
}

// AddRepoWith is AddRepo with registration options, such as shared git.
func (m *Manager) AddRepoWith(ctx context.Context, name, upstream string, opts repo.AddOptions) (*repo.Repo, error) {
	return repo.Add(ctx, m.Home, name, upstream, opts)
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

type SpawnOptions struct {
	Repo    string
	From    string // revision to start from; default HEAD of the mirror
	FromDir string // prepared checkout to snapshot as the base (instead of From)
	Base    string // existing base id to reuse (instead of From or FromDir)
	NoFetch bool   // resolve From against the mirror as is, fetching only if it is unknown
	ID      string // optional; random when empty
	Branch  string // optional; agent/<id> when empty
	Backend string // optional; backend.Default when empty
	// Share adds shared paths for this session on top of the repository's
	// (see SetShares). It shares them with every other session that shares
	// the same path, through the repository's one copy of it.
	Share []string
}

// Spawn creates a session: a private view of the base commit with a new
// branch checked out.
func (m *Manager) Spawn(ctx context.Context, opts SpawnOptions) (sess *session.Session, err error) {
	r, err := repo.Open(m.Home, opts.Repo)
	if err != nil {
		return nil, err
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
	n := 0
	for _, set := range []bool{opts.From != "", opts.FromDir != "", opts.Base != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return nil, errors.New("--from, --from-dir and --base are mutually exclusive")
	}
	var sha, head, source, baseDir string
	switch {
	case opts.FromDir != "":
		if sha, head, err = r.SnapshotDir(ctx, opts.FromDir); err != nil {
			return nil, err
		}
		source, _ = filepath.Abs(opts.FromDir)
		baseDir = r.BaseDir(sha)
	case opts.Base != "":
		sha = opts.Base
		if strings.ContainsAny(sha, `/\`) || strings.HasPrefix(sha, ".") {
			return nil, fmt.Errorf("invalid base id %q", sha)
		}
		baseDir = r.BaseDir(sha)
		if _, err := os.Stat(baseDir); err != nil {
			return nil, fmt.Errorf("repo %q has no base %q (see repo list -v)", r.Name, sha)
		}
		if h, herr := r.ReadHead(ctx, sha); herr == nil && h != sha {
			head = h
		}
	default:
		if sha, err = r.Resolve(ctx, orDefault(opts.From, "HEAD"), !opts.NoFetch); err != nil {
			return nil, err
		}
		if baseDir, err = r.EnsureBase(ctx, sha); err != nil {
			return nil, err
		}
	}

	// Pin before any view uses the base, so a gc in the mirrors can never
	// take objects out from under it. This also repairs older bases.
	if err := r.PinBase(ctx, sha); err != nil {
		return nil, err
	}

	shared, err := repo.MergeSharePaths(r.SharedPaths, opts.Share)
	if err != nil {
		return nil, err
	}
	id := opts.ID
	if id == "" {
		id = session.NewID()
	}
	if err := m.sessions.Reserve(id); err != nil {
		return nil, err
	}
	ws := backend.Workspace{ID: id, Repo: r.Name, BaseDir: baseDir, StateDir: m.sessions.Dir(id)}
	sharedGitDir := ""
	defer func() {
		if err != nil {
			_ = b.Remove(context.WithoutCancel(ctx), ws)
			if sharedGitDir != "" {
				_ = r.RemoveSessionGitDir(id)
			}
			_ = m.sessions.Delete(id)
		}
	}()

	// Seeding a share is permanent, so it waits until the id is ours.
	var shares []repo.Share
	if shares, err = r.EnsureShares(ctx, baseDir, shared); err != nil {
		return nil, err
	}
	ws.Shares = toBackendShares(shares)
	if ws.ViewPath, err = b.Create(ctx, ws); err != nil {
		return nil, fmt.Errorf("create view: %w", err)
	}
	if r.SharedGit {
		if sharedGitDir, err = r.AddSessionGitDir(ctx, id, baseDir, ws.ViewPath); err != nil {
			return nil, fmt.Errorf("shared git: %w", err)
		}
		// Only a process inside the view can change it (overlay), so the
		// switch from the base's .git to the shared one runs there.
		link := []string{"sh", "-c", `rm -rf .git && ln -s "$1" .git`, "sh", sharedGitDir}
		if err = m.run(ctx, b, ws, &session.Session{ID: id, Repo: r.Name}, link, nil, io.Discard, nil); err != nil {
			return nil, fmt.Errorf("shared git: link .git: %w", err)
		}
	}
	if err = m.excludeShares(ctx, b, ws, id, r.Name, shares); err != nil {
		return nil, fmt.Errorf("exclude shared paths: %w", err)
	}
	sess = &session.Session{
		ID:        id,
		Repo:      r.Name,
		Base:      sha,
		Head:      head,
		Source:    source,
		Branch:    orDefault(opts.Branch, "agent/"+id),
		Backend:   b.Name(),
		ViewPath:  ws.ViewPath,
		Shared:    shared,
		CreatedAt: time.Now().UTC(),
	}
	if err = m.run(ctx, b, ws, sess, []string{"git", "switch", "--quiet", "--create", sess.Branch}, nil, io.Discard, nil); err != nil {
		return nil, fmt.Errorf("create branch: %w", err)
	}
	if err = m.sessions.Save(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// FetchRepo updates the repository's mirror from upstream.
func (m *Manager) FetchRepo(ctx context.Context, name string) error {
	r, err := repo.Open(m.Home, name)
	if err != nil {
		return err
	}
	return r.Fetch(ctx)
}

// BaseInfo describes one base of a repository.
type BaseInfo struct {
	ID       string
	Head     string // commit the base is at
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
		b.Head, _ = r.ReadHead(ctx, id)
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
		if len(b.Sessions) > 0 {
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

// BaseHealth is the state of one base as CheckRepo sees it.
type BaseHealth struct {
	ID       string
	Sessions int
	Pinned   bool  // its objects are protected from gc in the mirrors
	Err      error // fsck failure: objects it needs are missing
}

// CheckRepo reports, for every base of the repository, whether it is pinned
// and whether git can still reach every object it needs. With fix set it
// pins unpinned bases first; it cannot bring back objects already gone.
func (m *Manager) CheckRepo(ctx context.Context, name string, fix bool) ([]BaseHealth, error) {
	r, err := repo.Open(m.Home, name)
	if err != nil {
		return nil, err
	}
	// Keep bases from being removed while they are checked or pinned.
	unlock, err := r.LockBases(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	bases, err := m.bases(ctx, r, false)
	if err != nil {
		return nil, err
	}
	var out []BaseHealth
	for _, b := range bases {
		h := BaseHealth{ID: b.ID, Sessions: len(b.Sessions), Pinned: b.Pinned}
		var pinErr error
		if fix && !b.Pinned {
			pinErr = r.PinBase(ctx, b.ID)
			if h.Pinned, err = r.Pinned(ctx, b.ID); err != nil {
				return nil, err
			}
		}
		h.Err = errors.Join(pinErr, r.Fsck(ctx, b.ID))
		out = append(out, h)
	}
	return out, nil
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

// Push pushes the session's branch to remote (default origin, the upstream).
func (m *Manager) Push(ctx context.Context, id, remote string, stdout, stderr io.Writer) error {
	sess, b, ws, err := m.load(id)
	if err != nil {
		return err
	}
	argv := []string{"git", "push", orDefault(remote, "origin"), "HEAD:refs/heads/" + sess.Branch}
	return m.run(ctx, b, ws, sess, argv, nil, stdout, stderr)
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

// excludeShares adds each untracked shared path to the view's
// info/exclude, so a session never commits shared content. A symlinked
// share (copy, clone) is a file to git, which a directory-only rule such
// as "node_modules/" in .gitignore would not match.
func (m *Manager) excludeShares(ctx context.Context, b backend.Backend, ws backend.Workspace, id, repoName string, shares []repo.Share) error {
	var patterns []string
	for _, s := range shares {
		if !s.Tracked {
			patterns = append(patterns, "/"+globEscaper.Replace(s.Path))
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	script := `f=$(git rev-parse --git-path info/exclude) && mkdir -p "$(dirname "$f")" && for p; do grep -qxF -- "$p" "$f" 2>/dev/null || printf '%s\n' "$p" >> "$f"; done`
	argv := append([]string{"sh", "-c", script, "sh"}, patterns...)
	return m.run(ctx, b, ws, &session.Session{ID: id, Repo: repoName}, argv, nil, io.Discard, nil)
}

var globEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)

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
