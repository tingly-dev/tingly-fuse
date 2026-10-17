// Command tingly-fuse gives many agents private, copy-on-write views of one
// git repository.
//
//	tingly-fuse repo add <name> <upstream>
//	tingly-fuse repo list [-v]
//	tingly-fuse repo remove <name> [--force]
//	tingly-fuse spawn <repo> [--from rev | --from-dir dir | --base id] [--id id] [--branch b] [--backend name]
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
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/clone"
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/fullcopy"
	_ "github.com/tingly-dev/tingly-fuse/internal/backend/overlay"
	"github.com/tingly-dev/tingly-fuse/internal/manager"
)

const usage = `usage:
  tingly-fuse repo add <name> <upstream>   register a repository
  tingly-fuse repo list [-v]               list registered repositories (-v: session and base counts)
  tingly-fuse repo remove <name> [--force] delete a repository (--force drops its sessions)
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
	case len(args) >= 1 && args[0] == "list":
		fs := flag.NewFlagSet("repo list", flag.ContinueOnError)
		verbose := fs.Bool("v", false, "also show session and base counts")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return runRepoList(m, *verbose)
	case len(args) >= 2 && args[0] == "remove":
		fs := flag.NewFlagSet("repo remove", flag.ContinueOnError)
		force := fs.Bool("force", false, "drop the repository's sessions first (unpushed work is lost)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: tingly-fuse repo remove <name> [--force]")
		}
		return m.RemoveRepo(ctx, args[1], *force)
	default:
		return fmt.Errorf("usage: tingly-fuse repo add <name> <upstream> | repo list [-v] | repo remove <name> [--force]")
	}
}

func runRepoList(m *manager.Manager, verbose bool) error {
	repos, err := m.Repos()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if !verbose {
		fmt.Fprintln(w, "NAME\tUPSTREAM")
		for _, r := range repos {
			fmt.Fprintf(w, "%s\t%s\n", r.Name, r.Upstream)
		}
		return w.Flush()
	}
	sessions, err := m.Sessions()
	if err != nil {
		return err
	}
	perRepo := map[string]int{}
	for _, s := range sessions {
		perRepo[s.Repo]++
	}
	fmt.Fprintln(w, "NAME\tSESSIONS\tBASES\tUPSTREAM")
	for _, r := range repos {
		bases, err := r.Bases()
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", r.Name, perRepo[r.Name], len(bases), r.Upstream)
	}
	return w.Flush()
}

func runSpawn(ctx context.Context, m *manager.Manager, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	var opts manager.SpawnOptions
	fs.StringVar(&opts.From, "from", "", "revision to start from (default: upstream HEAD)")
	fs.StringVar(&opts.FromDir, "from-dir", "", "snapshot this prepared checkout (dependencies installed, built) as the base")
	fs.StringVar(&opts.Base, "base", "", "reuse an existing base by id (see repo list -v), e.g. one made by --from-dir")
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
	if sess.Source != "" {
		fmt.Fprintf(os.Stderr, "source: %s\n", sess.Source)
	}
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Repo, s.Branch, shortBase(s.Base), s.Backend, s.ViewPath)
	}
	return w.Flush()
}

// shortBase abbreviates a commit sha; directory base ids are already short.
func shortBase(b string) string {
	if len(b) == 40 {
		return b[:12]
	}
	return b
}
