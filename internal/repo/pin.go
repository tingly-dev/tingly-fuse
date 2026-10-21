package repo

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tingly-dev/tingly-fuse/internal/gitx"
)

// Bases and the views layered on them hold no objects: they borrow them from
// the mirrors through alternates. git gc in a mirror only counts the
// mirror's own refs as roots, so once upstream deletes or rewrites a branch,
// or for objects that were never upstream at all (a prepared directory's
// unpushed commits and staged blobs), the next fetch's auto gc may delete
// objects a base still needs. Every base therefore pins, in each mirror it
// borrows from, everything its gitdirs point at:
//
//	refs/tingly/bases/<id>/<label>/keep     a commit whose tree is the index
//	                                        and whose parents are HEAD, every
//	                                        ref pointing at a commit and every
//	                                        commit in the reflogs
//	refs/tingly/bases/<id>/<label>/r/<ref>  each ref pointing at anything else
//	                                        (annotated tags), which a commit
//	                                        cannot have as a parent
//
// Folding commit refs into keep matters: a base of tingly-box carries about
// 900 remote-tracking branches and tags per gitdir.
//
// label is "root" for the base's own .git and m-<hash> for a submodule
// gitdir. The root keep ref is written last and marks the base as pinned.
const pinPrefix = "refs/tingly/bases/"

func pinRef(id, label, name string) string { return pinPrefix + id + "/" + label + "/" + name }

// PinBase pins the objects base id needs in the mirrors it borrows from. It
// is idempotent and cheap once the base is pinned, so callers can run it on
// every use; that also repairs bases created before pinning existed.
func (r *Repo) PinBase(ctx context.Context, id string) error {
	if pinned, err := r.Pinned(ctx, id); err != nil || pinned {
		return err
	}
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if pinned, err := r.Pinned(ctx, id); err != nil || pinned {
		return err
	}
	dirs, err := baseGitDirs(r.BaseDir(id))
	if err != nil {
		return err
	}
	// dirs[0] is the root; pin it last so a crash leaves the base unpinned.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := pinGitDir(ctx, dirs[i].gitdir, id, dirs[i].label); err != nil {
			return fmt.Errorf("pin base %s (%s): %w", id, dirs[i].label, err)
		}
	}
	return nil
}

// Pinned reports whether base id has been pinned.
func (r *Repo) Pinned(ctx context.Context, id string) (bool, error) {
	if _, err := os.Stat(r.BaseDir(id)); err != nil {
		return false, fmt.Errorf("repo %q has no base %q", r.Name, id)
	}
	out, err := gitx.Run(ctx, r.ObjectsDir(), "for-each-ref", "--format=x", pinRef(id, "root", "keep"))
	return out != "", err
}

// PinnedBases returns the ids of all pinned bases, with one git call.
func (r *Repo) PinnedBases(ctx context.Context) (map[string]bool, error) {
	if !r.IsGit() {
		return nil, nil // no mirrors, nothing to pin
	}
	out, err := gitx.Run(ctx, r.ObjectsDir(), "for-each-ref", "--format=%(refname)", pinPrefix)
	if err != nil {
		return nil, err
	}
	pinned := map[string]bool{}
	for _, ref := range strings.Split(out, "\n") {
		if id, ok := strings.CutSuffix(strings.TrimPrefix(ref, pinPrefix), "/root/keep"); ok {
			pinned[id] = true
		}
	}
	return pinned, nil
}

// Fsck checks that every gitdir of base id can reach all the objects its
// refs, index and reflogs need. It only reads.
func (r *Repo) Fsck(ctx context.Context, id string) error {
	dirs, err := baseGitDirs(r.BaseDir(id))
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if _, err := gitx.Run(ctx, d.gitdir, "--git-dir="+d.gitdir, "fsck", "--connectivity-only", "--no-dangling", "--no-progress"); err != nil {
			return fmt.Errorf("%s: %w", d.label, err)
		}
	}
	return nil
}

// lock serializes pinning within the repository, so two spawns do not
// both build the same base's pins.
func (r *Repo) lock() (func(), error) {
	return flock(filepath.Join(r.dir, ".pin.lock"), true)
}

// withMirrorLock runs fn holding mirror's own lock. Everything that writes a
// mirror's refs or config (fetch, pinning, unpinning) goes through it:
// concurrent fetches fail on each other's ref locks, and a fetch --prune
// and an unpin both need packed-refs.lock.
func withMirrorLock(mirror string, fn func() error) error {
	unlock, err := flock(filepath.Join(mirror, "tingly-fuse.lock"), true)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// fetchMirror fetches into mirror unless another fetch into it finished
// after since, which covers many spawns waiting on one fetch.
func fetchMirror(ctx context.Context, mirror string, since time.Time, args ...string) error {
	return withMirrorLock(mirror, func() error {
		if fi, err := os.Stat(filepath.Join(mirror, "FETCH_HEAD")); err == nil && fi.ModTime().After(since) && len(args) == 0 {
			return nil
		}
		_, err := gitx.Run(ctx, mirror, append([]string{"fetch", "--quiet", "--prune", "origin"}, args...)...)
		return err
	})
}

// LockBases guards the set of bases. Spawn holds it shared from choosing a
// base until its session is saved; removing bases holds it exclusively, so
// a base is never deleted between a spawn picking it and the session that
// records it.
func (r *Repo) LockBases(exclusive bool) (func(), error) {
	return flock(filepath.Join(r.dir, ".bases.lock"), exclusive)
}

// RemoveBase deletes base id and its pins in every mirror, after which the
// mirrors' gc may reclaim objects nothing else needs. Callers hold
// LockBases exclusively and have checked that no session uses the base.
func (r *Repo) RemoveBase(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid base id %q", id)
	}
	dir := r.BaseDir(id)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("repo %q has no base %q", r.Name, id)
	}
	// Move it out of sight first: a half-deleted base must never be found.
	trash, err := os.MkdirTemp(filepath.Join(r.dir, "bases"), ".tmp-rm-")
	if err != nil {
		return err
	}
	if err := os.Rename(dir, filepath.Join(trash, "tree")); err != nil {
		os.Remove(trash)
		return err
	}
	if err := r.unpin(ctx, id); err != nil {
		// Put it back: a base with some of its pins gone is still usable
		// and visible, so the next rm or prune can finish the job.
		if rerr := os.Rename(filepath.Join(trash, "tree"), dir); rerr == nil {
			os.Remove(trash)
		}
		return fmt.Errorf("remove base %s: %w", id, err)
	}
	return removeAll(trash)
}

// unpin deletes base id's pins in every mirror.
func (r *Repo) unpin(ctx context.Context, id string) error {
	if !r.IsGit() {
		return nil
	}
	mirrors, _ := filepath.Glob(filepath.Join(r.ModulesDir(), "*.git"))
	for _, m := range append([]string{r.ObjectsDir()}, mirrors...) {
		err := withMirrorLock(m, func() error {
			refs, err := gitx.Run(ctx, m, "for-each-ref", "--format=delete %(refname)", pinPrefix+id+"/")
			if err != nil || refs == "" {
				return err
			}
			_, err = gitx.RunWith(ctx, m, nil, refs+"\n", "update-ref", "--stdin")
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// SweepRemoved deletes what interrupted removals left in bases/.tmp-rm-*.
// Callers hold LockBases exclusively.
func (r *Repo) SweepRemoved() error {
	trash, _ := filepath.Glob(filepath.Join(r.dir, "bases", ".tmp-rm-*"))
	var errs []error
	for _, t := range trash {
		errs = append(errs, removeAll(t))
	}
	return errors.Join(errs...)
}

// removeAll is os.RemoveAll that also copes with read-only directories,
// which prepared directories (Go's module cache, for one) can contain.
func removeAll(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

type gitDir struct{ gitdir, label string }

// baseGitDirs returns the base's .git followed by every submodule gitdir
// under .git/modules, nested ones included.
func baseGitDirs(base string) ([]gitDir, error) {
	root := filepath.Join(base, ".git")
	out := []gitDir{{root, "root"}}
	mods, err := findGitDirs(filepath.Join(root, "modules"))
	if err != nil {
		return nil, err
	}
	for _, gd := range mods {
		rel, _ := filepath.Rel(root, gd)
		sum := sha256.Sum256([]byte(filepath.ToSlash(rel)))
		out = append(out, gitDir{gd, "m-" + hex.EncodeToString(sum[:8])})
	}
	return out, nil
}

// findGitDirs returns the gitdirs (directories with HEAD and objects) under
// dir, which need not exist.
func findGitDirs(dir string) ([]string, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, nil
	}
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if _, e := os.Stat(filepath.Join(p, "HEAD")); e == nil {
			if _, e := os.Stat(filepath.Join(p, "objects")); e == nil {
				dirs = append(dirs, p)
			}
		}
		return nil
	})
	return dirs, err
}

// mirrorOf returns the repository gitdir borrows objects from, or "" when
// it has no alternates (its objects are its own and need no pinning).
func mirrorOf(gitdir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(gitdir, "objects", "info", "alternates"))
	if os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			if !filepath.IsAbs(line) {
				line = filepath.Join(gitdir, "objects", line)
			}
			return filepath.Dir(filepath.Clean(line)), nil
		}
	}
	return "", nil
}

func pinGitDir(ctx context.Context, gitdir, id, label string) error {
	mirror, err := mirrorOf(gitdir)
	if err != nil || mirror == "" {
		return err
	}
	return withMirrorLock(mirror, func() error {
		if label != "root" {
			if err := setModuleRefspec(ctx, mirror); err != nil {
				return err
			}
		}
		return pinInto(ctx, gitdir, mirror, id, label)
	})
}

func pinInto(ctx context.Context, gitdir, mirror, id, label string) error {
	gd := "--git-dir=" + gitdir
	refs, err := gitx.Run(ctx, gitdir, gd, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return err
	}
	commits := []string{}
	if head, err := gitx.Run(ctx, gitdir, gd, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		commits = append(commits, head)
	}
	logged, err := reflogObjects(filepath.Join(gitdir, "logs"))
	if err != nil {
		return err
	}
	commits = append(commits, logged...)

	// Only objects the mirror actually has, with everything they reach, can
	// be pinned: a ref to an incomplete object makes every later gc in the
	// mirror fail. Whatever is missing was gone before pinning; fsck in
	// git check reports it.
	type ref struct{ sha, name string }
	var want []ref
	for _, line := range strings.Split(refs, "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok {
			want = append(want, ref{sha, name})
		}
	}
	shas := make([]string, 0, len(want)+len(commits))
	for _, w := range want {
		shas = append(shas, w.sha)
	}
	types, err := objectTypes(ctx, mirror, append(shas, commits...))
	if err != nil {
		return err
	}
	var present []string
	for sha := range types {
		present = append(present, sha)
	}
	whole, err := complete(ctx, mirror, present)
	if err != nil {
		return err
	}
	for sha := range types {
		if !whole[sha] {
			delete(types, sha)
		}
	}

	tree, err := indexTree(ctx, gitdir, mirror)
	if err != nil {
		return err
	}
	args := []string{"commit-tree", tree, "-m", "tingly-fuse: keep objects of base " + id + " (" + label + ")"}
	seen := map[string]bool{}
	for _, w := range want {
		commits = append(commits, w.sha)
	}
	for _, c := range commits {
		if types[c] == "commit" && !seen[c] {
			seen[c] = true
			args = append(args, "-p", c)
		}
	}
	env := []string{
		"GIT_AUTHOR_NAME=tingly-fuse", "GIT_AUTHOR_EMAIL=tingly-fuse@localhost",
		"GIT_COMMITTER_NAME=tingly-fuse", "GIT_COMMITTER_EMAIL=tingly-fuse@localhost",
	}
	keep, err := gitx.RunWith(ctx, mirror, env, "", args...)
	if err != nil {
		return err
	}

	var stdin strings.Builder
	for _, w := range want {
		if t := types[w.sha]; t != "" && t != "commit" {
			fmt.Fprintf(&stdin, "update %s %s\n", pinRef(id, label, "r/"+w.name), w.sha)
		}
	}
	fmt.Fprintf(&stdin, "update %s %s\n", pinRef(id, label, "keep"), keep)
	_, err = gitx.RunWith(ctx, mirror, nil, stdin.String(), "update-ref", "--stdin")
	return err
}

// indexTree writes, into mirror, a tree holding every blob gitdir's index
// refers to and returns its id. Conflicted entries are kept under a
// suffixed path so their stages survive too.
func indexTree(ctx context.Context, gitdir, mirror string) (string, error) {
	type entry struct{ mode, sha, path string }
	var all []entry
	var blobs []string
	if _, err := os.Stat(filepath.Join(gitdir, "index")); err == nil {
		out, err := gitx.Run(ctx, gitdir, "--git-dir="+gitdir, "ls-files", "--stage", "-z")
		if err != nil {
			return "", err
		}
		for _, e := range strings.Split(out, "\x00") {
			meta, path, ok := strings.Cut(e, "\t")
			if !ok {
				continue
			}
			f := strings.Fields(meta) // mode sha stage
			if len(f) != 3 {
				continue
			}
			if f[0] != "160000" { // gitlinks name a commit in another repository
				blobs = append(blobs, f[1])
			}
			if f[2] != "0" {
				path += "~stage" + f[2]
			}
			all = append(all, entry{f[0], f[1], path})
		}
	}
	// Blobs gc already took are left out, so the tree is complete.
	have, err := objectTypes(ctx, mirror, blobs)
	if err != nil {
		return "", err
	}
	var entries strings.Builder
	for _, e := range all {
		if e.mode == "160000" || have[e.sha] == "blob" {
			fmt.Fprintf(&entries, "%s %s 0\t%s\x00", e.mode, e.sha, e.path)
		}
	}
	tmp, err := os.CreateTemp("", "tingly-fuse-index-")
	if err != nil {
		return "", err
	}
	tmp.Close()
	os.Remove(tmp.Name()) // git wants to create the index itself
	defer os.Remove(tmp.Name())
	env := []string{"GIT_INDEX_FILE=" + tmp.Name()}
	if entries.Len() > 0 {
		if _, err := gitx.RunWith(ctx, mirror, env, entries.String(), "update-index", "-z", "--index-info"); err != nil {
			return "", err
		}
	}
	return gitx.RunWith(ctx, mirror, env, "", "write-tree")
}

// reflogObjects returns the object ids recorded in the reflogs under logs.
func reflogObjects(logs string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(logs, func(p string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			for _, sha := range fields[:min(2, len(fields))] {
				if strings.Trim(sha, "0") != "" {
					out = append(out, sha)
				}
			}
		}
		return sc.Err()
	})
	return out, err
}

// objectTypes looks objects up in repo and returns the type of each one it
// has.
func objectTypes(ctx context.Context, repo string, shas []string) (map[string]string, error) {
	types := map[string]string{}
	if len(shas) == 0 {
		return types, nil
	}
	out, err := gitx.RunWith(ctx, repo, nil, strings.Join(shas, "\n")+"\n", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		if sha, typ, ok := strings.Cut(line, " "); ok && typ != "missing" {
			types[sha] = typ
		}
	}
	return types, nil
}

// complete returns which of tips (objects mirror has) reach only objects
// mirror has. One walk answers the usual case where nothing is missing;
// only then are tips checked one by one.
func complete(ctx context.Context, mirror string, tips []string) (map[string]bool, error) {
	whole := map[string]bool{}
	if len(tips) == 0 {
		return whole, nil
	}
	missing := func(tips []string) (bool, error) {
		out, err := gitx.RunWith(ctx, mirror, nil, strings.Join(tips, "\n")+"\n",
			"rev-list", "--objects", "--missing=print", "--stdin")
		if err != nil {
			return false, err
		}
		return strings.HasPrefix(out, "?") || strings.Contains(out, "\n?"), nil
	}
	any, err := missing(tips)
	if err != nil {
		return nil, err
	}
	for _, t := range tips {
		whole[t] = !any
	}
	if !any {
		return whole, nil
	}
	for _, t := range tips {
		m, err := missing([]string{t})
		if err != nil {
			return nil, err
		}
		whole[t] = !m
	}
	return whole, nil
}
