// Command tingly-fuse gives many agents private, copy-on-write views of one
// git repository.
//
//	tingly-fuse repo add <name> <upstream> [--shared-git] [--share path]...
//	tingly-fuse repo share <name> [path...]
//	tingly-fuse repo unshare <name> <path>...
//	tingly-fuse repo list [-v]
//	tingly-fuse repo remove <name> [--force]
//	tingly-fuse spawn <repo> [--from rev | --from-dir dir | --base id] [--id id] [--branch b] [--backend name] [--share path]...
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
	"strings"
	"text/tabwriter"
	"time"

	tinglyfuse "github.com/tingly-dev/tingly-fuse"
)

const usage = `usage:
  tingly-fuse repo add <name> <upstream>   register a repository (--shared-git: sessions share
                                           branches, commits and config; --share path: sessions
                                           share that path, repeatable; see ADR 0010, 0011)
  tingly-fuse repo share <name> [path...]  share paths across new sessions (no path: list them)
  tingly-fuse repo unshare <name> <path>... stop sharing paths in new sessions
  tingly-fuse repo list [-v]               list registered repositories (-v: session and base counts)
  tingly-fuse repo fetch <name>            update the repository's mirror from upstream
  tingly-fuse repo check <name> [--fix]    verify every base is pinned and has all its objects
  tingly-fuse repo remove <name> [--force] delete a repository (--force drops its sessions)
  tingly-fuse base list <repo>             list bases with their sessions
  tingly-fuse base rm <repo> <id>          delete a base no session uses (id may be a unique prefix)
  tingly-fuse base prune <repo> [--dry-run] delete every base no session uses
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
	m, err := tinglyfuse.Open("")
	if err != nil {
		return err
	}

	cmd, args := args[0], args[1:]
	switch cmd {
	case "repo":
		return runRepo(ctx, m, args)
	case "base":
		return runBase(ctx, m, args)
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

func runRepo(ctx context.Context, m *tinglyfuse.Manager, args []string) error {
	switch {
	case len(args) >= 3 && args[0] == "add":
		fs := flag.NewFlagSet("repo add", flag.ContinueOnError)
		shared := fs.Bool("shared-git", false, "make every session a worktree of one shared repository: branches, commits and config are visible to all sessions")
		var shares listFlag
		fs.Var(&shares, "share", "path every session shares (repeatable, or comma-separated)")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: tingly-fuse repo add <name> <upstream> [--shared-git] [--share path]...")
		}
		r, err := m.AddRepoWith(ctx, args[1], args[2], tinglyfuse.AddOptions{SharedGit: *shared, SharedPaths: shares})
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
	case len(args) == 2 && args[0] == "fetch":
		return m.FetchRepo(ctx, args[1])
	case len(args) >= 2 && args[0] == "check":
		fs := flag.NewFlagSet("repo check", flag.ContinueOnError)
		fix := fs.Bool("fix", false, "pin bases that are not pinned yet")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: tingly-fuse repo check <name> [--fix]")
		}
		return runRepoCheck(ctx, m, args[1], *fix)
	case len(args) >= 2 && (args[0] == "share" || args[0] == "unshare"):
		remove := args[0] == "unshare"
		if remove && len(args) == 2 {
			return fmt.Errorf("usage: tingly-fuse repo unshare <name> <path>...")
		}
		var paths []string
		if len(args) > 2 || remove {
			r, err := m.SetShares(args[1], args[2:], remove)
			if err != nil {
				return err
			}
			paths = r.SharedPaths
		} else {
			var err error
			if paths, err = m.Shares(args[1]); err != nil {
				return err
			}
		}
		for _, p := range paths {
			fmt.Println(p)
		}
		return nil
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
		return fmt.Errorf("usage: tingly-fuse repo add <name> <upstream> [--shared-git] [--share path]... | repo share <name> [path...] | repo unshare <name> <path>... | repo list [-v] | repo fetch <name> | repo check <name> [--fix] | repo remove <name> [--force]")
	}
}

func runRepoList(m *tinglyfuse.Manager, verbose bool) error {
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
	fmt.Fprintln(w, "NAME\tSESSIONS\tBASES\tGIT\tUPSTREAM")
	for _, r := range repos {
		bases, err := r.Bases()
		if err != nil {
			return err
		}
		git := "private"
		if r.SharedGit {
			git = "shared"
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", r.Name, perRepo[r.Name], len(bases), git, r.Upstream)
	}
	return w.Flush()
}

func runBase(ctx context.Context, m *tinglyfuse.Manager, args []string) error {
	const usage = "usage: tingly-fuse base list <repo> | base rm <repo> <id> | base prune <repo> [--dry-run]"
	switch {
	case len(args) == 2 && args[0] == "list":
		bases, err := m.Bases(ctx, args[1])
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "BASE\tHEAD\tCREATED\tPINNED\tSESSIONS")
		for _, b := range bases {
			sessions := strings.Join(b.Sessions, ",")
			if sessions == "" {
				sessions = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\n", b.ID, shortBase(b.Head), b.Created.UTC().Format(time.RFC3339), b.Pinned, sessions)
		}
		return w.Flush()
	case len(args) == 3 && args[0] == "rm":
		return m.RemoveBase(ctx, args[1], args[2])
	case len(args) >= 2 && args[0] == "prune":
		fs := flag.NewFlagSet("base prune", flag.ContinueOnError)
		dryRun := fs.Bool("dry-run", false, "only list the bases that would be deleted")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(usage)
		}
		pruned, err := m.PruneBases(ctx, args[1], *dryRun)
		for _, id := range pruned {
			fmt.Println(id)
		}
		return err
	default:
		return errors.New(usage)
	}
}

func runRepoCheck(ctx context.Context, m *tinglyfuse.Manager, name string, fix bool) error {
	bases, err := m.CheckRepo(ctx, name, fix)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "BASE\tSESSIONS\tPINNED\tOBJECTS")
	bad := 0
	for _, b := range bases {
		state := "ok"
		if b.Err != nil {
			state = "MISSING"
			bad++
		}
		if !b.Pinned {
			bad++
		}
		fmt.Fprintf(w, "%s\t%d\t%v\t%s\n", shortBase(b.ID), b.Sessions, b.Pinned, state)
	}
	w.Flush()
	for _, b := range bases {
		if b.Err != nil {
			fmt.Fprintf(os.Stderr, "\n%s:\n%v\n", b.ID, b.Err)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d problem(s) found", bad)
	}
	return nil
}

func runSpawn(ctx context.Context, m *tinglyfuse.Manager, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	var opts tinglyfuse.SpawnOptions
	fs.StringVar(&opts.From, "from", "", "revision to start from (default: upstream HEAD)")
	fs.StringVar(&opts.FromDir, "from-dir", "", "snapshot this prepared checkout (dependencies installed, built) as the base")
	fs.StringVar(&opts.Base, "base", "", "reuse an existing base by id (see repo list -v), e.g. one made by --from-dir")
	fs.BoolVar(&opts.NoFetch, "no-fetch", false, "resolve --from against the local mirror without fetching first")
	fs.StringVar(&opts.ID, "id", "", "session id (default: random)")
	fs.StringVar(&opts.Branch, "branch", "", "branch to create (default: agent/<id>)")
	fs.StringVar(&opts.Backend, "backend", "", fmt.Sprintf("view backend %v (default %q)", tinglyfuse.Backends(), tinglyfuse.DefaultBackend()))
	fs.Var((*listFlag)(&opts.Share), "share", "also share this path with other sessions sharing it (repeatable, or comma-separated)")
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

func runExec(ctx context.Context, m *tinglyfuse.Manager, args []string) error {
	if len(args) < 3 || args[1] != "--" {
		return fmt.Errorf("usage: tingly-fuse exec <session> -- <cmd> [args...]")
	}
	return m.Exec(ctx, args[0], args[2:], os.Stdin, os.Stdout, os.Stderr)
}

func runPush(ctx context.Context, m *tinglyfuse.Manager, args []string) error {
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

func runList(m *tinglyfuse.Manager) error {
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

// listFlag collects a repeatable flag; each value may also be a
// comma-separated list.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}
