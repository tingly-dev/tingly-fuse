package manager_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/manager"
)

func ids(t *testing.T, m *manager.Manager, selector ...string) []string {
	t.Helper()
	sessions, err := m.SessionsWithLabels(selector)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range sessions {
		out = append(out, s.ID)
	}
	return out
}

func TestSessionLabels(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	add := func(id string, labels map[string]string) {
		t.Helper()
		if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: id, Labels: labels}); err != nil {
			t.Fatal(err)
		}
	}
	add("a", map[string]string{"task": "login", "owner": "x"})
	add("b", map[string]string{"task": "billing"})
	add("c", nil)

	if got := ids(t, m); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("all = %v", got)
	}
	if got := ids(t, m, "task"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("task = %v", got)
	}
	if got := ids(t, m, "task=login", "owner=x"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("task=login,owner=x = %v", got)
	}
	if got := ids(t, m, "task=nope"); len(got) != 0 {
		t.Errorf("task=nope = %v", got)
	}

	// Labels persist, change and disappear.
	s, err := m.SetLabels("c", map[string]string{"task": "login"}, nil)
	if err != nil || s.Labels["task"] != "login" {
		t.Fatalf("set: %v %v", s, err)
	}
	if got := ids(t, m, "task=login"); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("after set = %v", got)
	}
	if _, err := m.SetLabels("a", map[string]string{"task": "refactor"}, []string{"owner", "missing"}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Session("a")
	if !reflect.DeepEqual(got.Labels, map[string]string{"task": "refactor"}) {
		t.Errorf("a labels = %v", got.Labels)
	}
	if s, err := m.SetLabels("a", nil, []string{"task"}); err != nil || s.Labels != nil {
		t.Errorf("removing the last label: %v %v", s, err)
	}

	// Mistakes are errors and leave things as they were.
	if _, err := m.SetLabels("a", map[string]string{"k": "v"}, []string{"k"}); err == nil {
		t.Error("set and remove of one key accepted")
	}
	if _, err := m.SetLabels("a", map[string]string{"bad key": "v"}, nil); err == nil {
		t.Error("invalid key accepted")
	}
	if _, err := m.SetLabels("nope", map[string]string{"k": "v"}, nil); err == nil {
		t.Error("label on a missing session accepted")
	}
	if _, err := m.SessionsWithLabels([]string{"bad key"}); err == nil {
		t.Error("invalid selector accepted")
	}
	if _, err := m.Git().AddWorktree(ctx, manager.WorktreeOptions{Repo: "demo", ID: "d", Labels: map[string]string{"bad key": "v"}}); err == nil {
		t.Error("spawn with an invalid label succeeded")
	}
	if got := ids(t, m); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("sessions after the failed spawn = %v", got)
	}
}
