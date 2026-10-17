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
	return repo.Add(ctx, m.Home, name, upstream)
}

// Repos lists registered repositories.
func (m *Manager) Repos() ([]*repo.Repo, error) { return repo.List(m.Home) }

type SpawnOptions struct {
	Repo    string
	From    string // revision to start from; default HEAD of the mirror
	ID      string // optional; random when empty
	Branch  string // optional; agent/<id> when empty
	Backend string // optional; backend.Default when empty
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
	sha, err := r.Resolve(ctx, orDefault(opts.From, "HEAD"))
	if err != nil {
		return nil, err
	}
	baseDir, err := r.EnsureBase(ctx, sha)
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
	defer func() {
		if err != nil {
			_ = b.Remove(context.WithoutCancel(ctx), ws)
			_ = m.sessions.Delete(id)
		}
	}()

	if ws.ViewPath, err = b.Create(ctx, ws); err != nil {
		return nil, fmt.Errorf("create view: %w", err)
	}
	sess = &session.Session{
		ID:        id,
		Repo:      r.Name,
		Base:      sha,
		Branch:    orDefault(opts.Branch, "agent/"+id),
		Backend:   b.Name(),
		ViewPath:  ws.ViewPath,
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

// Drop removes the session's view and metadata. Unpushed work is lost.
func (m *Manager) Drop(ctx context.Context, id string) error {
	_, b, ws, err := m.load(id)
	if err != nil {
		return err
	}
	return errors.Join(b.Remove(ctx, ws), m.sessions.Delete(id))
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
	return sess, b, ws, nil
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
