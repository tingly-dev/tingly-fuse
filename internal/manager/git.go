package manager

// The git layer (ADR 0013): repositories with an upstream, whose bases are
// commits of a shared mirror, whose sessions each get a branch to push, and
// which may share one .git (ADR 0010). Everything else, shared paths
// included, is the generic layer in manager.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/repo"
	"github.com/tingly-dev/tingly-fuse/internal/session"
)

// Git is the git layer of a Manager.
type Git struct{ m *Manager }

// Git returns the manager's git layer.
func (m *Manager) Git() *Git { return &Git{m} }

// Clone registers git repository upstream under name and mirrors it. An
// empty name is taken from upstream, as git clone names its directory.
func (g *Git) Clone(ctx context.Context, upstream, name string, opts repo.AddOptions) (*repo.Repo, error) {
	if name == "" {
		name = RepoNameFromURL(upstream)
	}
	return repo.Add(ctx, g.m.Home, name, upstream, opts)
}

// RepoNameFromURL is the name git clone would give upstream's directory:
// its last path component without .git.
func RepoNameFromURL(upstream string) string {
	s := strings.TrimRight(upstream, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(s, ".git")
}

// WorktreeOptions say where a session of a git repository starts, which
// branch it works on and what it shares.
type WorktreeOptions struct {
	Repo    string
	Commit  string // commit-ish to start from; default HEAD of the mirror
	FromDir string // prepared checkout to snapshot as the base (instead of Commit)
	Base    string // existing base id to reuse (instead of Commit or FromDir)
	NoFetch bool   // resolve Commit against the mirror as is, fetching only if it is unknown
	ID      string // optional; random when empty
	Branch  string // optional; agent/<id> when empty
	Backend string // optional; backend.Default when empty
	// Share adds shared paths for this session on top of the repository's;
	// see SpawnOptions.Share.
	Share []string
}

// AddWorktree creates a session of a git repository, like git worktree
// add: a private view of the base commit with a new branch checked out.
func (g *Git) AddWorktree(ctx context.Context, opts WorktreeOptions) (*session.Session, error) {
	r, err := g.open(opts.Repo)
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
	for _, set := range []bool{opts.Commit != "", opts.FromDir != "", opts.Base != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return nil, errors.New("a commit, --from-dir and --base are mutually exclusive")
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
		if baseDir, err = existingBase(r, sha); err != nil {
			return nil, err
		}
		if h, herr := r.ReadHead(ctx, sha); herr == nil && h != sha {
			head = h
		}
	default:
		if sha, err = r.Resolve(ctx, orDefault(opts.Commit, "HEAD"), !opts.NoFetch); err != nil {
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

	sess := &session.Session{ID: opts.ID, Base: sha, Head: head, Source: source}
	return g.m.newSession(ctx, r, b, baseDir, sess, opts.Share, func(ws backend.Workspace, sess *session.Session, shares []repo.Share) error {
		if r.SharedGit {
			dir, err := r.AddSessionGitDir(ctx, sess.ID, baseDir, ws.ViewPath)
			if err != nil {
				return fmt.Errorf("shared git: %w", err)
			}
			// Only a process inside the view can change it (overlay), so the
			// switch from the base's .git to the shared one runs there.
			link := []string{"sh", "-c", `rm -rf .git && ln -s "$1" .git`, "sh", dir}
			if err := g.m.run(ctx, b, ws, sess, link, nil, io.Discard, nil); err != nil {
				return fmt.Errorf("shared git: link .git: %w", err)
			}
		}
		if err := g.m.excludeShares(ctx, b, ws, sess, shares); err != nil {
			return fmt.Errorf("exclude shared paths: %w", err)
		}
		sess.Branch = orDefault(opts.Branch, "agent/"+sess.ID)
		if err := g.m.run(ctx, b, ws, sess, []string{"git", "switch", "--quiet", "--create", sess.Branch}, nil, io.Discard, nil); err != nil {
			return fmt.Errorf("create branch: %w", err)
		}
		return nil
	})
}

// Fetch updates the repository's mirror from upstream.
func (g *Git) Fetch(ctx context.Context, name string) error {
	r, err := g.open(name)
	if err != nil {
		return err
	}
	return r.Fetch(ctx)
}

// Worktrees lists the sessions of git repositories, or of repository name
// when it is set, oldest first.
func (g *Git) Worktrees(name string) ([]*session.Session, error) {
	if name != "" {
		if _, err := g.open(name); err != nil {
			return nil, err
		}
	}
	sessions, err := g.m.sessions.List()
	if err != nil {
		return nil, err
	}
	gitRepo := map[string]bool{}
	var out []*session.Session
	for _, s := range sessions {
		if name != "" && s.Repo != name {
			continue
		}
		isGit, ok := gitRepo[s.Repo]
		if !ok {
			r, err := repo.Open(g.m.Home, s.Repo)
			isGit = err == nil && r.IsGit()
			gitRepo[s.Repo] = isGit
		}
		if isGit {
			out = append(out, s)
		}
	}
	return out, nil
}

// RemoveWorktree drops a session of a git repository, like git worktree
// remove; see Manager.Drop.
func (g *Git) RemoveWorktree(ctx context.Context, id string) error {
	sess, err := g.m.sessions.Get(id)
	if err != nil {
		return err
	}
	if _, err := g.open(sess.Repo); err != nil {
		return err
	}
	return g.m.Drop(ctx, id)
}

// BaseHealth is the state of one base as Fsck sees it.
type BaseHealth struct {
	ID       string
	Sessions int
	Pinned   bool  // its objects are protected from gc in the mirrors
	Err      error // fsck failure: objects it needs are missing
}

// Fsck reports, for every base of the repository, whether it is pinned
// and whether git fsck can still reach every object it needs. With fix set
// it pins unpinned bases first; it cannot bring back objects already gone.
func (g *Git) Fsck(ctx context.Context, name string, fix bool) ([]BaseHealth, error) {
	r, err := g.open(name)
	if err != nil {
		return nil, err
	}
	// Keep bases from being removed while they are checked or pinned.
	unlock, err := r.LockBases(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	bases, err := g.m.bases(ctx, r, false)
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

// Push pushes the session's branch to remote (default origin, the upstream).
func (g *Git) Push(ctx context.Context, id, remote string, stdout, stderr io.Writer) error {
	sess, b, ws, err := g.m.load(id)
	if err != nil {
		return err
	}
	if _, err := g.open(sess.Repo); err != nil {
		return err
	}
	argv := []string{"git", "push", orDefault(remote, "origin"), "HEAD:refs/heads/" + sess.Branch}
	return g.m.run(ctx, b, ws, sess, argv, nil, stdout, stderr)
}

// open opens a registered git repository.
func (g *Git) open(name string) (*repo.Repo, error) {
	r, err := repo.Open(g.m.Home, name)
	if err != nil {
		return nil, err
	}
	if !r.IsGit() {
		return nil, fmt.Errorf("repo %q is a plain directory, not a git repository", name)
	}
	return r, nil
}

// excludeShares adds each untracked shared path to the view's
// info/exclude, so a session never commits shared content. A symlinked
// share (copy, clone) is a file to git, which a directory-only rule such
// as "node_modules/" in .gitignore would not match.
func (m *Manager) excludeShares(ctx context.Context, b backend.Backend, ws backend.Workspace, sess *session.Session, shares []repo.Share) error {
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
	return m.run(ctx, b, ws, sess, argv, nil, io.Discard, nil)
}

var globEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
