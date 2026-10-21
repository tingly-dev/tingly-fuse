// Command tingly-fuse gives many agents private, copy-on-write views of one
// directory or git repository (ADR 0013).
//
// Any directory:
//
//	tingly-fuse base add <name> <dir> [--share path]...
//	tingly-fuse spawn <name> [--base id] [--id id] [--backend name] [--share path]...
//
// Git repositories:
//
//	tingly-fuse git add <name> <upstream> [--shared-git] [--share path]...
//	tingly-fuse git spawn <name> [--from rev | --from-dir dir | --base id] [--branch b] [...]
//	tingly-fuse git push <session> [--remote origin]
//
// Either:
//
//	tingly-fuse exec <session> -- <cmd> [args...]
//	tingly-fuse share <name> [path...] / unshare <name> <path>...
//	tingly-fuse drop <session> / list / remove <name> [--force]
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
any directory:
  tingly-fuse base add <name> <dir>        snapshot dir as the base new sessions of name start
                                           from, registering name first (--share path: sessions
                                           share that path, repeatable)
  tingly-fuse spawn <name> [flags]         create a session and print its id

git repositories:
  tingly-fuse git add <name> <upstream>    register a repository (--shared-git: sessions share
                                           branches, commits and config; --share path: as above)
  tingly-fuse git spawn <name> [flags]     create a session on its own branch and print its id
  tingly-fuse git push <session> [flags]   push the session's branch
  tingly-fuse git fetch <name>             update the repository's mirror from upstream
  tingly-fuse git check <name> [--fix]     verify every base is pinned and has all its objects

either:
  tingly-fuse exec <session> -- <cmd>...   run a command inside a session's view
  tingly-fuse drop <session>               delete a session (unpushed work is lost)
  tingly-fuse list                         list sessions
  tingly-fuse share <name> [path...]       share paths across new sessions (no path: list them)
  tingly-fuse unshare <name> <path>...     stop sharing paths in new sessions
  tingly-fuse base list [name]             list repositories, or one repository's bases
  tingly-fuse base rm <name> <id>          delete a base no session uses (id may be a unique prefix)
  tingly-fuse base prune <name> [--dry-run] delete every base no session uses
  tingly-fuse remove <name> [--force]      delete a repository (--force drops its sessions)

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
	case "base":
		return runBase(ctx, m, args)
	case "spawn":
		return runSpawn(ctx, m, args)
	case "git":
		return runGit(ctx, m, args)
	case "exec":
		return runExec(ctx, m, args)
	case "share", "unshare":
		return runShare(m, cmd == "unshare", args)
	case "drop":
		if len(args) != 1 {
			return fmt.Errorf("usage: tingly-fuse drop <session>")
		}
		return m.Drop(ctx, args[0])
	case "list":
		return runList(m)
	case "remove":
		fs := flag.NewFlagSet("remove", flag.ContinueOnError)
		force := fs.Bool("force", false, "drop the repository's sessions first (unpushed work is lost)")
		if len(args) == 0 {
			return fmt.Errorf("usage: tingly-fuse remove <name> [--force]")
		}
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: tingly-fuse remove <name> [--force]")
		}
		return m.RemoveRepo(ctx, args[0], *force)
	case "repo", "push":
		return fmt.Errorf("%s moved (ADR 0013): repo add, repo fetch, repo check and push are now git add, git fetch, git check and git push; repo share, unshare, remove and list are share, unshare, remove and base list", cmd)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func runGit(ctx context.Context, m *tinglyfuse.Manager, args []string) error {
	const usage = "usage: tingly-fuse git add <name> <upstream> [--shared-git] [--share path]... | git spawn <name> [flags] | git push <session> [--remote r] | git fetch <name> | git check <name> [--fix]"
	g := m.Git()
	switch {
	case len(args) >= 3 && args[0] == "add":
		fs := flag.NewFlagSet("git add", flag.ContinueOnError)
		shared := fs.Bool("shared-git", false, "make every session a worktree of one shared repository: branches, commits and config are visible to all sessions")
		var shares listFlag
		fs.Var(&shares, "share", "path every session shares (repeatable, or comma-separated)")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: tingly-fuse git add <name> <upstream> [--shared-git] [--share path]...")
		}
		r, err := g.Add(ctx, args[1], args[2], tinglyfuse.AddOptions{SharedGit: *shared, SharedPaths: shares})
		if err != nil {
			return err
		}
		fmt.Println(r.Name)
		return nil
	case len(args) >= 1 && args[0] == "spawn":
		return runGitSpawn(ctx, g, args[1:])
	case len(args) >= 1 && args[0] == "push":
		fs := flag.NewFlagSet("git push", flag.ContinueOnError)
		remote := fs.String("remote", "origin", "remote to push to")
		if len(args) < 2 {
			return fmt.Errorf("usage: tingly-fuse git push <session> [--remote r]")
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		return g.Push(ctx, args[1], *remote, os.Stdout, os.Stderr)
	case len(args) == 2 && args[0] == "fetch":
		return g.Fetch(ctx, args[1])
	case len(args) >= 2 && args[0] == "check":
		fs := flag.NewFlagSet("git check", flag.ContinueOnError)
		fix := fs.Bool("fix", false, "pin bases that are not pinned yet")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: tingly-fuse git check <name> [--fix]")
		}
		return runGitCheck(ctx, g, args[1], *fix)
	default:
		return errors.New(usage)
	}
}

func runShare(m *tinglyfuse.Manager, remove bool, args []string) error {
	if len(args) == 0 || remove && len(args) == 1 {
		if remove {
			return fmt.Errorf("usage: tingly-fuse unshare <name> <path>...")
		}
		return fmt.Errorf("usage: tingly-fuse share <name> [path...]")
	}
	var paths []string
	if len(args) > 1 {
		r, err := m.SetShares(args[0], args[1:], remove)
		if err != nil {
			return err
		}
		paths = r.SharedPaths
	} else {
		var err error
		if paths, err = m.Shares(args[0]); err != nil {
			return err
		}
	}
	for _, p := range paths {
		fmt.Println(p)
	}
	return nil
}

func runRepos(m *tinglyfuse.Manager) error {
	repos, err := m.Repos()
	if err != nil {
		return err
	}
	sessions, err := m.Sessions()
	if err != nil {
		return err
	}
	perRepo := map[string]int{}
	for _, s := range sessions {
		perRepo[s.Repo]++
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tSESSIONS\tBASES\tSOURCE")
	for _, r := range repos {
		bases, err := r.Bases()
		if err != nil {
			return err
		}
		kind, source := tinglyfuse.KindDir, r.Source
		if r.IsGit() {
			kind, source = tinglyfuse.KindGit, r.Upstream
			if r.SharedGit {
				kind = "git (shared)"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\n", r.Name, kind, perRepo[r.Name], len(bases), source)
	}
	return w.Flush()
}

func runBase(ctx context.Context, m *tinglyfuse.Manager, args []string) error {
	const usage = "usage: tingly-fuse base add <name> <dir> [--share path]... | base list [name] | base rm <name> <id> | base prune <name> [--dry-run]"
	switch {
	case len(args) >= 3 && args[0] == "add":
		fs := flag.NewFlagSet("base add", flag.ContinueOnError)
		var shares listFlag
		fs.Var(&shares, "share", "path every session shares (repeatable, or comma-separated); only when registering")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("usage: tingly-fuse base add <name> <dir> [--share path]...")
		}
		_, id, err := m.AddDir(ctx, args[1], args[2], tinglyfuse.DirOptions{SharedPaths: shares})
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	case len(args) == 1 && args[0] == "list":
		return runRepos(m)
	case len(args) == 2 && args[0] == "list":
		r, err := m.Repo(args[1])
		if err != nil {
			return err
		}
		bases, err := m.Bases(ctx, args[1])
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		if r.IsGit() {
			fmt.Fprintln(w, "BASE\tHEAD\tCREATED\tPINNED\tSESSIONS")
		} else {
			fmt.Fprintln(w, "BASE\tCREATED\tCURRENT\tSESSIONS")
		}
		for _, b := range bases {
			sessions := strings.Join(b.Sessions, ",")
			if sessions == "" {
				sessions = "-"
			}
			created := b.Created.UTC().Format(time.RFC3339)
			if r.IsGit() {
				fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\n", b.ID, shortBase(b.Head), created, b.Pinned, sessions)
			} else {
				fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", b.ID, created, b.Current, sessions)
			}
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

func runGitCheck(ctx context.Context, g *tinglyfuse.Git, name string, fix bool) error {
	bases, err := g.Check(ctx, name, fix)
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

func backendFlag(fs *flag.FlagSet, v *string) {
	fs.StringVar(v, "backend", "", fmt.Sprintf("view backend %v (default %q)", tinglyfuse.Backends(), tinglyfuse.DefaultBackend()))
}

func runSpawn(ctx context.Context, m *tinglyfuse.Manager, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	var opts tinglyfuse.SpawnOptions
	fs.StringVar(&opts.Base, "base", "", "start from this base (see base list <name>; default: the latest snapshot)")
	fs.StringVar(&opts.ID, "id", "", "session id (default: random)")
	backendFlag(fs, &opts.Backend)
	fs.Var((*listFlag)(&opts.Share), "share", "also share this path with other sessions sharing it (repeatable, or comma-separated)")
	if len(args) == 0 {
		return fmt.Errorf("usage: tingly-fuse spawn <name> [flags]")
	}
	opts.Repo = args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tingly-fuse spawn <name> [flags]")
	}
	sess, err := m.Spawn(ctx, opts)
	if err != nil {
		return err
	}
	fmt.Println(sess.ID)
	fmt.Fprintf(os.Stderr, "view: %s\nbase: %s\n", sess.ViewPath, sess.Base)
	return nil
}

func runGitSpawn(ctx context.Context, g *tinglyfuse.Git, args []string) error {
	fs := flag.NewFlagSet("git spawn", flag.ContinueOnError)
	var opts tinglyfuse.GitSpawnOptions
	fs.StringVar(&opts.From, "from", "", "revision to start from (default: upstream HEAD)")
	fs.StringVar(&opts.FromDir, "from-dir", "", "snapshot this prepared checkout (dependencies installed, built) as the base")
	fs.StringVar(&opts.Base, "base", "", "reuse an existing base by id (see base list <name>), e.g. one made by --from-dir")
	fs.BoolVar(&opts.NoFetch, "no-fetch", false, "resolve --from against the local mirror without fetching first")
	fs.StringVar(&opts.ID, "id", "", "session id (default: random)")
	fs.StringVar(&opts.Branch, "branch", "", "branch to create (default: agent/<id>)")
	backendFlag(fs, &opts.Backend)
	fs.Var((*listFlag)(&opts.Share), "share", "also share this path with other sessions sharing it (repeatable, or comma-separated)")
	if len(args) == 0 {
		return fmt.Errorf("usage: tingly-fuse git spawn <name> [flags]")
	}
	opts.Repo = args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tingly-fuse git spawn <name> [flags]")
	}
	sess, err := g.Spawn(ctx, opts)
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

func runList(m *tinglyfuse.Manager) error {
	sessions, err := m.Sessions()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tREPO\tBRANCH\tBASE\tBACKEND\tVIEW")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Repo, orDash(s.Branch), shortBase(s.Base), s.Backend, s.ViewPath)
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

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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
