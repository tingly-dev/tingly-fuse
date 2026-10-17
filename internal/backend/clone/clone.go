// Package clone is the macOS backend: each session's tree is an APFS
// clonefile(2) of the base checkout. Clones share data blocks with the base
// until written, so spawning is cheap regardless of repository size.
//
// Views live at sessions/<id>/root, a different path per session; tools
// inside the view should read TINGLY_WORKSPACE rather than hard-code it.
// When the volume cannot clone (not APFS, or base and state on different
// volumes), Create falls back to a plain copy so the session still works.
//
// The backend registers itself, and becomes the default, only on darwin.
package clone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
)

// errUnsupported means the filesystem cannot clone src to dst.
var errUnsupported = errors.New("clonefile not supported")

type Backend struct{}

func (Backend) Name() string { return "clone" }

func (Backend) Create(ctx context.Context, ws backend.Workspace) (string, error) {
	root := filepath.Join(ws.StateDir, "root")
	err := cloneTree(ws.BaseDir, root)
	if errors.Is(err, errUnsupported) {
		// Anything a failed clone left behind would make cp nest the copy.
		if err := os.RemoveAll(root); err != nil {
			return "", err
		}
		err = copyTree(ctx, ws.BaseDir, root)
	}
	if err != nil {
		return "", fmt.Errorf("clone base: %w", err)
	}
	return root, nil
}

func (Backend) Command(ctx context.Context, ws backend.Workspace, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = ws.ViewPath
	return cmd, nil
}

func (Backend) Remove(ctx context.Context, ws backend.Workspace) error {
	return os.RemoveAll(filepath.Join(ws.StateDir, "root"))
}

func copyTree(ctx context.Context, src, dst string) error {
	out, err := exec.CommandContext(ctx, "cp", "-a", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}
