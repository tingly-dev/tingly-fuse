package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
	"github.com/tingly-dev/tingly-fuse/internal/session"
)

// Checkpoint freezes a session's current tree as a new base of its
// repository and returns the base's id; new sessions can start from it
// (git worktree add --base, or spawn --base). The whole tree is copied, with
// reflinks where the filesystem has them, uncommitted and untracked files
// included, as --from-dir does for a prepared directory.
//
// The copy is as consistent as the tree is while it runs: processes still
// writing in the session can leave it half done, so stop them first. Paths
// the session shares with other sessions are left out of the base, and are
// shared again by sessions started from it. Sessions that share git cannot be
// checkpointed, and neither can views the host cannot read.
//
// A base no session uses is removed by the next base prune, so start the
// sessions that need it before pruning.
func (m *Manager) Checkpoint(ctx context.Context, id string) (string, error) {
	sess, err := m.sessions.Get(id)
	if err != nil {
		return "", err
	}
	r, err := repo.Open(m.Home, sess.Repo)
	if err != nil {
		return "", err
	}
	// Keep the new base from being pruned before a session uses it.
	unlock, err := r.LockBases(false)
	if err != nil {
		return "", err
	}
	defer unlock()
	return m.checkpoint(ctx, r, id)
}

// checkpoint is Checkpoint with the bases lock held by the caller.
func (m *Manager) checkpoint(ctx context.Context, r *repo.Repo, id string) (string, error) {
	sess, b, ws, err := m.load(id)
	if err != nil {
		return "", err
	}
	if r.SharedGit {
		return "", fmt.Errorf("session %s shares git with its repository, whose worktree state a checkpoint cannot copy; checkpoint is not supported for --shared-git yet", id)
	}
	if hv, ok := b.(backend.HostVisible); ok && !hv.HostVisible(ws) {
		return "", fmt.Errorf("session %s has a view only commands run by tingly-fuse can see (backend %s), so it cannot be copied from the host; spawn with the overlay or fuse-overlay backend", id, b.Name())
	}
	// Mounts the view again if it was lost; the command itself does nothing.
	if err := m.run(ctx, b, ws, sess, []string{"true"}, nil, io.Discard, nil); err != nil {
		return "", err
	}
	var base string
	if r.IsGit() {
		if base, _, err = r.SnapshotDir(ctx, ws.ViewPath); err != nil {
			return "", err
		}
	} else if base, err = r.SnapshotTree(ctx, ws.ViewPath); err != nil {
		return "", err
	}
	if err := dropShares(r.BaseDir(base), sess.Shared); err != nil {
		_ = os.RemoveAll(r.BaseDir(base))
		return "", err
	}
	if r.IsGit() {
		// Before any view uses the base, as every other way of making one.
		if err := r.PinBase(ctx, base); err != nil {
			return "", err
		}
	}
	return base, nil
}

// dropShares deletes the shared paths from a base made from a view: there
// they showed the shared copy, and a session started from the base shares
// them again itself.
func dropShares(base string, shared []string) error {
	for _, p := range shared {
		if err := os.RemoveAll(filepath.Join(base, filepath.FromSlash(p))); err != nil {
			return fmt.Errorf("leave shared path %s out of the checkpoint: %w", p, err)
		}
	}
	return nil
}

// ForkOptions say what a forked session gets. Everything but ID may be left
// empty.
type ForkOptions struct {
	ID      string   // random when empty
	Branch  string   // git: new branch; agent/<id> when empty
	Backend string   // the forked session's backend; the parent's when empty
	Share   []string // as in SpawnOptions
}

// Fork starts a new session from a checkpoint of session id: it sees the
// parent's tree as it is now, uncommitted work included, and then the two
// diverge. The checkpoint stays as a base (see Checkpoint); the new session
// uses it, so it is kept until the new session is dropped.
func (m *Manager) Fork(ctx context.Context, id string, opts ForkOptions) (*session.Session, error) {
	parent, err := m.sessions.Get(id)
	if err != nil {
		return nil, err
	}
	backendName := orDefault(opts.Backend, parent.Backend)
	r, err := repo.Open(m.Home, parent.Repo)
	if err != nil {
		return nil, err
	}
	// Held until the new session uses the base, so a prune cannot take it.
	unlock, err := r.LockBases(false)
	if err != nil {
		return nil, err
	}
	base, err := m.checkpoint(ctx, r, id)
	if err != nil {
		unlock()
		return nil, err
	}
	var sess *session.Session
	if r.IsGit() {
		sess, err = m.Git().AddWorktree(ctx, WorktreeOptions{Repo: parent.Repo, Base: base, ID: opts.ID, Branch: opts.Branch, Backend: backendName, Share: opts.Share})
	} else {
		sess, err = m.Spawn(ctx, SpawnOptions{Repo: parent.Repo, Base: base, ID: opts.ID, Backend: backendName, Share: opts.Share})
	}
	unlock()
	if err != nil {
		// Nothing uses the checkpoint; do not leave it behind.
		return nil, errors.Join(err, m.RemoveBase(ctx, r.Name, base))
	}
	return sess, nil
}
