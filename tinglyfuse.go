// Package tinglyfuse is the Go API of tingly-fuse: everything the
// tingly-fuse command does, callable from a program. The command itself
// is built on this package only.
//
// It has two layers, like the command (ADR 0013). The generic layer works
// on any directory: snapshot it, spawn private views of the snapshot, share
// paths between them, run commands in them.
//
//	m, _ := tinglyfuse.Open("")  // TINGLY_FUSE_HOME, or ~/.tingly-fuse
//	m.AddDir(ctx, "data", "/srv/data", tinglyfuse.DirOptions{SharedPaths: []string{"cache"}})
//	s, _ := m.Spawn(ctx, tinglyfuse.SpawnOptions{Repo: "data"})
//	m.Exec(ctx, s.ID, []string{"make"}, nil, os.Stdout, os.Stderr)
//	m.Drop(ctx, s.ID)
//
// The git layer, m.Git(), adds an upstream mirror, bases at commits,
// a branch per session to push, and shared .git:
//
//	m.Git().Add(ctx, "box", "https://github.com/tingly-dev/tingly-box",
//		tinglyfuse.AddOptions{SharedGit: true, SharedPaths: []string{"node_modules/.cache"}})
//	s, _ := m.Git().Spawn(ctx, tinglyfuse.GitSpawnOptions{Repo: "box", Share: []string{".env"}})
//	m.Exec(ctx, s.ID, []string{"go", "test", "./..."}, nil, os.Stdout, os.Stderr)
//	m.Git().Push(ctx, s.ID, "", os.Stdout, os.Stderr)
//	m.Drop(ctx, s.ID)
//
// Importing it registers every backend this platform supports.
package tinglyfuse

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/clone"
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/fullcopy"
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/overlay"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
	"github.com/tingly-dev/tingly-fuse/internal/session"
)

type (
	// Manager owns the repositories and sessions under one home directory.
	Manager = manager.Manager
	// Git is a Manager's git layer.
	Git = manager.Git
	// SpawnOptions say which snapshot a session of a dir repository starts
	// from and what it shares.
	SpawnOptions = manager.SpawnOptions
	// GitSpawnOptions say where a session of a git repository starts, its
	// branch and what it shares.
	GitSpawnOptions = manager.GitSpawnOptions
	// DirOptions are the settings a dir repository is registered with.
	DirOptions = repo.DirOptions
	// AddOptions are the settings a git repository is registered with.
	AddOptions = repo.AddOptions
	// Repo is a registered repository: a plain directory or a git
	// repository.
	Repo = repo.Repo
	// Session is one agent's view of a repository.
	Session = session.Session
	// BaseInfo describes a base and the sessions using it.
	BaseInfo = manager.BaseInfo
	// BaseHealth is the result of checking one base.
	BaseHealth = manager.BaseHealth
)

// Repository kinds, as in Repo.Kind.
const (
	KindDir = repo.KindDir
	KindGit = repo.KindGit
)

// Environment variables set for every process run inside a view.
const (
	EnvWorkspace = manager.EnvWorkspace
	EnvSession   = manager.EnvSession
	EnvRepo      = manager.EnvRepo
)

// New returns a Manager for the given home directory.
func New(home string) *Manager { return manager.New(home) }

// Open returns a Manager for home, or for the default home (TINGLY_FUSE_HOME,
// else ~/.tingly-fuse) when home is empty.
func Open(home string) (*Manager, error) {
	if home == "" {
		var err error
		if home, err = manager.DefaultHome(); err != nil {
			return nil, err
		}
	}
	return manager.New(home), nil
}

// Backends lists the backends available on this platform.
func Backends() []string { return backend.Names() }

// CheckBackend reports whether backend name can run sessions on this host
// (overlay needs mount and, unprivileged, user namespaces), by creating and
// entering a throwaway view.
func CheckBackend(ctx context.Context, name string) error {
	b, err := backend.Get(name)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "tingly-fuse-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	ws := backend.Workspace{ID: "probe", Repo: ".probe-" + filepath.Base(tmp), BaseDir: filepath.Join(tmp, "base"), StateDir: filepath.Join(tmp, "state")}
	for _, d := range []string{ws.BaseDir, ws.StateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if ws.ViewPath, err = b.Create(ctx, ws); err != nil {
		return err
	}
	defer func() {
		b.Remove(context.WithoutCancel(ctx), ws)
		if !strings.HasPrefix(ws.ViewPath, tmp) {
			os.Remove(ws.ViewPath) // overlay's mount point under its view root
		}
	}()
	cmd, err := b.Command(ctx, ws, []string{"true"})
	if err != nil {
		return err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// DefaultBackend is the backend sessions use when no backend is given.
func DefaultBackend() string { return backend.Default }
