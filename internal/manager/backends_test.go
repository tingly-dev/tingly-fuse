package manager_test

import (
	"context"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
)

// eachBackend runs fn once per backend usable on this host, with that
// backend as the default, so scenarios written against the manager cover
// every way a view can be built. Backends that cannot run here (overlay
// without mount namespaces, for one) are skipped, not failed.
func eachBackend(t *testing.T, fn func(t *testing.T)) {
	for _, name := range platformBackends() {
		t.Run(name, func(t *testing.T) {
			if err := usable(t, name); err != nil {
				t.Skipf("backend %s unavailable: %v", name, err)
			}
			saved := backend.Default
			backend.Default = name
			t.Cleanup(func() { backend.Default = saved })
			fn(t)
		})
	}
}

// dropAll drops every session when the test ends, so backends with
// long-lived state (overlay's holder processes) do not outlive it.
func dropAll(t *testing.T, m *manager.Manager) {
	t.Cleanup(func() {
		sessions, _ := m.Sessions()
		for _, s := range sessions {
			m.Drop(context.Background(), s.ID)
		}
	})
}
