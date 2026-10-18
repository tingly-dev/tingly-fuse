package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tingly-dev/tingly-fuse/internal/gitx"
)

// ModulesDir holds one bare mirror per submodule URL, shared by every base
// of the repository, so submodule objects follow the same rule as the
// superproject's: they live outside the base, reached through alternates.
func (r *Repo) ModulesDir() string { return filepath.Join(r.dir, "modules") }

func (r *Repo) moduleMirror(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(r.ModulesDir(), hex.EncodeToString(sum[:8])+".git")
}

// initSubmodules checks out every submodule of the tree at dir, recursively,
// from local mirrors. Each submodule's .git/modules/<name> gets alternates
// into its mirror instead of its own packs, and its origin stays the real
// URL so agents can fetch and push from inside a view as usual.
func (r *Repo) initSubmodules(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".gitmodules")); err != nil {
		return nil
	}
	// init resolves relative URLs against the superproject's origin and
	// records them as submodule.<name>.url.
	if _, err := gitx.Run(ctx, dir, "submodule", "--quiet", "init"); err != nil {
		return err
	}
	out, err := gitx.Run(ctx, dir, "config", "--local", "--get-regexp", `^submodule\..*\.url$`)
	if err != nil {
		return nil // no submodules initialized (empty .gitmodules)
	}
	for _, line := range strings.Split(out, "\n") {
		key, url, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".url")
		path, err := gitx.Run(ctx, dir, "config", "--file", ".gitmodules", "submodule."+name+".path")
		if err != nil {
			return fmt.Errorf("submodule %q: no path in .gitmodules", name)
		}
		if err := r.checkoutSubmodule(ctx, dir, name, path, url); err != nil {
			return fmt.Errorf("submodule %s: %w", path, err)
		}
		if err := r.initSubmodules(ctx, filepath.Join(dir, path)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) checkoutSubmodule(ctx context.Context, dir, name, path, url string) error {
	want, err := gitx.Run(ctx, dir, "rev-parse", "HEAD:"+path)
	if err != nil {
		return err
	}
	mirror, err := r.ensureModuleMirror(ctx, url, want)
	if err != nil {
		return err
	}
	// Clone from the mirror over file:// with it as reference: nothing is
	// transferred or hardlinked, the new repo only gets alternates.
	key := "submodule." + name + ".url"
	if _, err := gitx.Run(ctx, dir, "config", key, "file://"+mirror); err != nil {
		return err
	}
	_, updErr := gitx.Run(ctx, dir, "-c", "protocol.file.allow=always", "submodule", "--quiet", "update", "--reference", mirror, "--", path)
	if _, err := gitx.Run(ctx, dir, "config", key, url); err != nil {
		return err
	}
	if updErr != nil {
		return updErr
	}
	_, err = gitx.Run(ctx, filepath.Join(dir, path), "remote", "set-url", "origin", url)
	return err
}

// ensureModuleMirror returns the mirror for url, cloning it or fetching
// into it when it lacks commit want.
func (r *Repo) ensureModuleMirror(ctx context.Context, url, want string) (string, error) {
	mirror := r.moduleMirror(url)
	if _, err := os.Stat(mirror); err != nil {
		if err := os.MkdirAll(r.ModulesDir(), 0o755); err != nil {
			return "", err
		}
		tmp, err := os.MkdirTemp(r.ModulesDir(), ".tmp-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		if _, err := gitx.Run(ctx, tmp, "clone", "--quiet", "--mirror", url, "m.git"); err != nil {
			return "", err
		}
		if err := setModuleRefspec(ctx, filepath.Join(tmp, "m.git")); err != nil {
			return "", err
		}
		if err := os.Rename(filepath.Join(tmp, "m.git"), mirror); err != nil {
			if _, statErr := os.Stat(mirror); statErr != nil {
				return "", err
			}
		}
	}
	if _, err := gitx.Run(ctx, mirror, "cat-file", "-e", want+"^{commit}"); err == nil {
		return mirror, nil
	}
	if err := setModuleRefspec(ctx, mirror); err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, mirror, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, mirror, "cat-file", "-e", want+"^{commit}"); err != nil {
		// Commits not on any branch (e.g. a force-pushed-away pin) can still
		// be fetched by id from servers that allow it.
		if _, ferr := gitx.Run(ctx, mirror, "fetch", "--quiet", "origin", want); ferr != nil {
			return "", fmt.Errorf("commit %s not found in %s", want, url)
		}
	}
	return mirror, nil
}

// setModuleRefspec limits what fetch --prune may delete in a module mirror to
// branches and tags. A --mirror clone fetches +refs/*:refs/*, which would
// prune the refs/tingly/ pins (see pin.go) on every fetch. Run before each
// fetch so mirrors made before pinning existed are fixed too.
func setModuleRefspec(ctx context.Context, mirror string) error {
	for _, args := range [][]string{
		{"config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"},
		{"config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*"},
	} {
		if _, err := gitx.Run(ctx, mirror, args...); err != nil {
			return err
		}
	}
	return nil
}
