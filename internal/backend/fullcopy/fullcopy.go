// Package fullcopy is the baseline backend: each session gets a full copy of the
// base checkout. It works on any platform and is what the other backends are
// benchmarked against.
package fullcopy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
)

func init() { backend.Register(Backend{}) }

type Backend struct{}

func (Backend) Name() string { return "copy" }

func (Backend) Create(ctx context.Context, ws backend.Workspace) (string, error) {
	root := filepath.Join(ws.StateDir, "root")
	// cp -a keeps modes, symlinks and timestamps; the .git inside the base
	// only holds HEAD/index/refs, objects are reached through alternates.
	out, err := exec.CommandContext(ctx, "cp", "-a", ws.BaseDir, root).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("copy base: %w: %s", err, out)
	}
	return root, backend.LinkShares(root, ws.Shares)
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
