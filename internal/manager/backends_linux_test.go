package manager_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/backend/overlay"
)

// Importing overlay makes it the default; the other tests in this package
// are written against copy.
func init() { backend.Default = "copy" }

func platformBackends() []string { return []string{"copy", "overlay"} }

func usable(t *testing.T, name string) error {
	if name != "overlay" {
		return nil
	}
	tmp := t.TempDir()
	t.Setenv(overlay.EnvViewRoot, filepath.Join(tmp, "workspace"))
	ws := backend.Workspace{ID: "probe", Repo: "probe", BaseDir: filepath.Join(tmp, "base"), StateDir: filepath.Join(tmp, "state")}
	os.MkdirAll(ws.BaseDir, 0o755)
	b := overlay.Backend{}
	var err error
	if ws.ViewPath, err = b.Create(context.Background(), ws); err != nil {
		return err
	}
	defer b.Remove(context.Background(), ws)
	cmd, err := b.Command(context.Background(), ws, []string{"true"})
	if err != nil {
		return err
	}
	return cmd.Run()
}
