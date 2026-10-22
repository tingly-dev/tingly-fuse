package manager_test

import (
	"testing"

	"github.com/tingly-dev/tingly-fuse/internal/manager"
)

// git clone without a name names the repository the way git names its
// directory.
func TestRepoNameFromURL(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/tingly-dev/tingly-box":      "tingly-box",
		"https://github.com/tingly-dev/tingly-box.git":  "tingly-box",
		"https://github.com/tingly-dev/tingly-box.git/": "tingly-box",
		"git@github.com:tingly-dev/tingly-box.git":      "tingly-box",
		"git@host:box.git":                              "box",
		"/srv/git/upstream.git":                         "upstream",
		"../box":                                        "box",
	} {
		if got := manager.RepoNameFromURL(url); got != want {
			t.Errorf("RepoNameFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}
