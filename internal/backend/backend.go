// Package backend defines how a session's private view of a repository is
// created and entered. Each platform provides its own implementation:
//
//   - copy:    full copy of the base (baseline, works everywhere)
//   - overlay: Linux mount namespace + overlayfs at a fixed path
//   - clone:   macOS APFS clonefile copy-on-write
//
// The manager owns everything git- and session-related; a backend only
// answers "where does this session's tree live and how do I run a process
// that sees it".
package backend

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"sync"
)

// Workspace is what a backend gets to work with for one session.
type Workspace struct {
	// ID is the session id.
	ID string
	// Repo is the registered repository name.
	Repo string
	// BaseDir is the read-only materialized checkout of the base commit,
	// shared by every session spawned from that commit. Its .git uses
	// alternates pointing outside BaseDir, so object reads never write into it.
	BaseDir string
	// StateDir is private to this session; the backend may keep any data
	// (upper/work dirs, a cloned tree) under it.
	StateDir string
	// ViewPath is where processes started through Command see the tree.
	// Set by Create and persisted by the manager.
	ViewPath string
}

// Backend creates, enters and removes a session's view.
type Backend interface {
	// Name is the identifier stored in session metadata.
	Name() string
	// Create prepares the session's private view of BaseDir and returns the
	// path at which processes will see it.
	Create(ctx context.Context, ws Workspace) (viewPath string, err error)
	// Command returns a command that runs argv inside the view with its
	// working directory at the view root. The caller wires stdio and env.
	Command(ctx context.Context, ws Workspace, argv []string) (*exec.Cmd, error)
	// Remove deletes everything Create made. It must be safe to call on a
	// partially created workspace.
	Remove(ctx context.Context, ws Workspace) error
}

var (
	mu       sync.RWMutex
	registry = map[string]Backend{}
)

// Register makes a backend available by name. Platform backends call it
// from init.
func Register(b Backend) {
	mu.Lock()
	defer mu.Unlock()
	registry[b.Name()] = b
}

// Get returns the backend registered under name.
func Get(name string) (Backend, error) {
	mu.RLock()
	defer mu.RUnlock()
	b, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (available: %v)", name, namesLocked())
	}
	return b, nil
}

// Names lists registered backends.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	return namesLocked()
}

func namesLocked() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Default is the backend used when none is requested. Platform files may
// override it from init.
var Default = "copy"
