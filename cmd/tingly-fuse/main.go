// Command tingly-fuse gives many agents private, copy-on-write views of one
// git repository.
//
//	tingly-fuse repo add <name> <upstream>
//	tingly-fuse repo list
//	tingly-fuse spawn <repo> [--from rev] [--id id] [--branch b] [--backend name]
//	tingly-fuse exec <session> -- <cmd> [args...]
//	tingly-fuse push <session> [--remote origin]
//	tingly-fuse drop <session>
//	tingly-fuse list
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"text/tabwriter"

	"github.com/tingly-dev/tingly-fuse/internal/backend"
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/fullcopy"
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/overlay"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
)

const usage = `usage:
  tingly-fuse repo add <name> <upstream>   register a repository
  tingly-fuse repo list                    list registered repositories
  tingly-fuse spawn <repo> [flags]         create a session and print its id
  tingly-fuse exec <session> -- <cmd>...   run a command inside a session's view
  tingly-fuse push <session> [flags]       push the session's branch
  tingly-fuse drop <session>               delete a session (unpushed work is lost)
  tingly-fuse list                         list sessions

environment:
  TINGLY_FUSE_HOME   state directory (default ~/.tingly-fuse)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := run(ctx, os.Args[1:])
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		os.Exit(exitErr.ExitCode()) // propagate the inner command's status
	case errors.Is(err, flag.ErrHelp):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "tingly-fuse:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return flag.ErrHelp
	}
	home, err := manager.DefaultHome()
	if err != nil {
		return err
	}
	m := manager.New(home)

	cmd, args := args[0], args[1:]
	switch cmd {
	case "repo":
		return runRepo(ctx, m, args)
	case "spawn":
		return runSpawn(ctx, m, args)
	case "exec":
		return runExec(ctx, m, args)
	case "push":
		return runPush(ctx, m, args)
	case "drop":
		if len(args) != 1 {
			return fmt.Errorf("usage: tingly-fuse drop <session>")
		}
		return m.Drop(ctx, args[0])
	case "list":
		return runList(m)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func runRepo(ctx context.Context, m *manager.Manager, args []string) error {
	switch {
	case len(args) == 3 && args[0] == "add":
		r, err := m.AddRepo(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Println(r.Name)
		return nil
	case len(args) == 1 && args[0] == "list":
		repos, err := m.Repos()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tUPSTREAM")
		for _, r := range repos {
			fmt.Fprintf(w, "%s\t%s\n", r.Name, r.Upstream)
		}
		return w.Flush()
	default:
		return fmt.Errorf("usage: tingly-fuse repo add <name> <upstream> | repo list")
	}
}

func runSpawn(ctx context.Context, m *manager.Manager, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	var opts manager.SpawnOptions
	fs.StringVar(&opts.From, "from", "", "revision to start from (default: upstream HEAD)")
	fs.StringVar(&opts.ID, "id", "", "session id (default: random)")
	fs.StringVar(&opts.Branch, "branch", "", "branch to create (default: agent/<id>)")
	fs.StringVar(&opts.Backend, "backend", "", fmt.Sprintf("view backend %v (default %q)", backend.Names(), backend.Default))
	if len(args) == 0 {
		return fmt.Errorf("usage: tingly-fuse spawn <repo> [flags]")
	}
	opts.Repo = args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	sess, err := m.Spawn(ctx, opts)
	if err != nil {
		return err
	}
	fmt.Println(sess.ID)
	fmt.Fprintf(os.Stderr, "view: %s\nbranch: %s\nbase: %s\n", sess.ViewPath, sess.Branch, sess.Base)
	return nil
}

func runExec(ctx context.Context, m *manager.Manager, args []string) error {
	if len(args) < 3 || args[1] != "--" {
		return fmt.Errorf("usage: tingly-fuse exec <session> -- <cmd> [args...]")
	}
	return m.Exec(ctx, args[0], args[2:], os.Stdin, os.Stdout, os.Stderr)
}

func runPush(ctx context.Context, m *manager.Manager, args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	remote := fs.String("remote", "origin", "remote to push to")
	if len(args) == 0 {
		return fmt.Errorf("usage: tingly-fuse push <session> [--remote r]")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	return m.Push(ctx, args[0], *remote, os.Stdout, os.Stderr)
}

func runList(m *manager.Manager) error {
	sessions, err := m.Sessions()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tREPO\tBRANCH\tBASE\tBACKEND\tVIEW")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Repo, s.Branch, s.Base[:12], s.Backend, s.ViewPath)
	}
	return w.Flush()
}
