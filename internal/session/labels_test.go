package session

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestLabelValidation(t *testing.T) {
	ok := []map[string]string{
		nil,
		{"task": "fix-login"},
		{"example.com/owner": "agent 7", "a": ""},
		{"x": strings.Repeat("v", maxLabelValue)},
	}
	for _, l := range ok {
		if err := ValidateLabels(l); err != nil {
			t.Errorf("%v: %v", l, err)
		}
	}
	bad := []map[string]string{
		{"": "v"},
		{"-a": "v"},
		{"a b": "v"},
		{"a=b": "v"},
		{strings.Repeat("k", maxLabelKey+1): "v"},
		{"k": strings.Repeat("v", maxLabelValue+1)},
		{"k": "line\nbreak"},
	}
	for _, l := range bad {
		if err := ValidateLabels(l); err == nil {
			t.Errorf("%q accepted", l)
		}
	}
}

func TestParseLabels(t *testing.T) {
	got, err := ParseLabels([]string{"a=1", "b=", "c=x=y"})
	if err != nil || got["a"] != "1" || got["b"] != "" || got["c"] != "x=y" || len(got) != 3 {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := ParseLabels([]string{"novalue"}); err == nil {
		t.Error("label without = accepted")
	}
}

func TestSelector(t *testing.T) {
	labels := map[string]string{"task": "login", "owner": "a", "empty": ""}
	for _, tc := range []struct {
		terms []string
		want  bool
	}{
		{nil, true},
		{[]string{"task"}, true},
		{[]string{"task=login"}, true},
		{[]string{"task=other"}, false},
		{[]string{"missing"}, false},
		{[]string{"empty"}, true},
		{[]string{"empty="}, true},
		{[]string{"task=login", "owner=a"}, true},
		{[]string{"task=login", "owner=b"}, false},
	} {
		sel, err := ParseSelector(tc.terms)
		if err != nil {
			t.Fatal(err)
		}
		if got := sel.Match(labels); got != tc.want {
			t.Errorf("%v: match = %v, want %v", tc.terms, got, tc.want)
		}
	}
	if _, err := ParseSelector([]string{"bad key"}); err == nil {
		t.Error("invalid selector accepted")
	}
	if got := FormatLabels(labels); got != "empty=,owner=a,task=login" {
		t.Errorf("format = %q", got)
	}
	if got := FormatLabels(nil); got != "-" {
		t.Errorf("format of none = %q", got)
	}
}

// Concurrent updates of one session each keep their own change.
func TestUpdateIsSerialized(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Reserve("s"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Update("s", func(s *Session) error {
				if s.Labels == nil {
					s.Labels = map[string]string{}
				}
				s.Labels[fmt.Sprintf("k%d", i)] = "v"
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := store.Get("s")
	if err != nil || len(got.Labels) != n {
		t.Errorf("labels after %d updates: %d, %v", n, len(got.Labels), err)
	}
	if _, err := store.Update("nope", func(*Session) error { return nil }); err == nil {
		t.Error("update of a missing session succeeded")
	}
}
