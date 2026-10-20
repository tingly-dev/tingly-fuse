// Package tinglyfuse is the Go API of tingly-fuse: everything the
// tingly-fuse command does, callable from a program. The command itself
// is built on this package only.
//
//	m, _ := tinglyfuse.Open("")  // TINGLY_FUSE_HOME, or ~/.tingly-fuse
//	m.AddRepoWith(ctx, "box", "https://github.com/tingly-dev/tingly-box",
//		tinglyfuse.AddOptions{SharedPaths: []string{"node_modules/.cache"}})
//	s, _ := m.Spawn(ctx, tinglyfuse.SpawnOptions{Repo: "box", Share: []string{".env"}})
//	m.Exec(ctx, s.ID, []string{"go", "test", "./..."}, nil, os.Stdout, os.Stderr)
//	m.Push(ctx, s.ID, "", os.Stdout, os.Stderr)
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
	// SpawnOptions say where a session starts and what it shares.
	SpawnOptions = manager.SpawnOptions
	// AddOptions are the settings a repository is registered with.
	AddOptions = repo.AddOptions
	// Repo is a registered repository.
	Repo = repo.Repo
	// Session is one agent's view of a repository.
	Session = session.Session
	// BaseInfo describes a base and the sessions using it.
	BaseInfo = manager.BaseInfo
	// BaseHealth is the result of checking one base.
	BaseHealth = manager.BaseHealth
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

// DefaultBackend is the backend sessions use when SpawnOptions.Backend is
// empty.
func DefaultBackend() string { return backend.Default }
