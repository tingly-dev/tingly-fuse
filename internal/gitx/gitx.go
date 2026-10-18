// Package gitx runs the git command line. tingly-fuse shells out to git
// instead of using a Go git library so that alternates, fsmonitor and other
// on-disk behaviour match what agents' own git invocations will see.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Run executes git with args in dir and returns trimmed stdout.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	return RunWith(ctx, dir, nil, "", args...)
}

// RunWith is Run with extra environment variables and stdin.
func RunWith(ctx context.Context, dir string, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
