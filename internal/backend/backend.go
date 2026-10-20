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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	// Shares are paths in the view that show a host directory or file
	// instead of the base's copy, live and writable, so every session
	// sharing it sees the others' changes at once.
	Shares []Share
}

// Share is one shared path of a workspace.
type Share struct {
	// Path is relative to the view root, slash-separated, never .git.
	Path string
	// Source is the directory or file on the host every session sees.
	Source string
	// Tracked is set when git tracks something at Path in the base.
	Tracked bool
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

// LinkShares replaces each shared path in view with a symlink to its
// source, for backends that cannot bind mount. Tracked paths are refused:
// git would see the symlink as the tracked content being replaced, and an
// agent committing that would delete it upstream.
func LinkShares(view string, shares []Share) error {
	for _, s := range shares {
		if s.Tracked {
			return fmt.Errorf("shared path %s is tracked by git, and this backend shares paths with symlinks, which git sees as replacing it; use the overlay backend or share untracked paths only", s.Path)
		}
		dst, err := InView(view, s.Path)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(s.Source, dst); err != nil {
			return fmt.Errorf("share %s: %w", s.Path, err)
		}
	}
	return nil
}

// InView returns where rel is under view, failing if any directory on the
// way is a symlink, which could lead outside the view.
func InView(view, rel string) (string, error) {
	dir := view
	parts := strings.Split(rel, "/")
	for _, p := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, p)
		fi, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			break
		} else if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("shared path %s: %s is not a directory in the view", rel, strings.TrimPrefix(dir, view+string(filepath.Separator)))
		}
	}
	return filepath.Join(view, filepath.FromSlash(rel)), nil
}
