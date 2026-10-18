package repo

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	_, err := gitx.Run(ctx, r.ObjectsDir(), "rev-parse", "--verify", "--quiet", pinRef(id, "root", "keep"))
	return err == nil, nil
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

// lock serializes pinning within the repository: concurrent update-ref on
// the same refs would otherwise fail on each other's lock files.
func (r *Repo) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(r.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
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
	if label != "root" {
		if err := setModuleRefspec(ctx, mirror); err != nil {
			return err
		}
	}
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

	// Only objects the mirror actually has can be pinned; anything else
	// was already gone before this base was made.
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
	var entries strings.Builder
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
			if f[2] != "0" {
				path += "~stage" + f[2]
			}
			fmt.Fprintf(&entries, "%s %s 0\t%s\x00", f[0], f[1], path)
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
	return gitx.RunWith(ctx, mirror, env, "", "write-tree", "--missing-ok")
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
