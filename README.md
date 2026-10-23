# tingly-fuse

**English** | [简体中文](README.zh-CN.md)

Run many coding agents on one git repository at the same time, without a worktree per agent and without container orchestration.

Each agent gets a **private, copy-on-write view** of the repository. It starts from a commit you choose, commits only to its own branch, and pushes that branch to your remote. Merging and review happen there, as normal pull requests.

The views work on any directory too, git or not: `base add` snapshots it and `spawn` gives each agent its own copy. The git features (mirror, commits, branches, push, shared `.git`) live under `tingly-fuse git` ([ADR 0013](.design/decisions/0013-generic-layer-and-git-subcommand.md)).

## Why

When several agents share one checkout, they overwrite each other's files, index and `HEAD`. The usual fixes have costs:

- **A worktree or clone per agent** gives every agent a different path. Build caches that key on absolute paths (Go's, for example) miss on every new path, so each agent pays for a cold build. On [tingly-box](https://github.com/tingly-dev/tingly-box) that is about 150 s per agent.
- **A container per agent** solves isolation but brings images, orchestration and volume management along with it.

tingly-fuse separates what can be shared from what must be private:

| Shared by all sessions | Private to each session |
|---|---|
| Git objects (one bare mirror per repo) | Working-tree changes |
| Unchanged files of the base checkout | Index, `HEAD`, branch |
| Build caches that do not key on the path | Anything the agent writes |

Every session has its own path, `~/.tingly-fuse/sessions/<id>/root`, on every platform. Build caches keyed on absolute paths need the tool's own option to ignore them: with `go build -trimpath` and a shared `GOCACHE`, building tingly-box in a second session takes about 10 s instead of about 90 s. On Linux, creating a session takes about 30 ms and roughly 100 KB of disk. See the [benchmarks](bench/README.md) and [ADR 0014](.design/decisions/0014-per-session-view-path.md).

## How it works

```
                 ~/.tingly-fuse/repos/<repo>/
                 ├── objects.git      one bare mirror; every git object lives here
                 └── bases/<commit>/  read-only checkout of a commit (.git uses alternates)
                            │
          ┌─────────────────┼─────────────────┐
          ▼                 ▼                 ▼
      session A         session B         session C        copy-on-write views
 sessions/A/root    sessions/B/root    sessions/C/root    (one path per session)
   branch agent/A     branch agent/B     branch agent/C
          │                 │                 │
          └──────── git push to upstream ─────┘  →  merge via PR on the remote
```

1. `git clone` registers an upstream and mirrors it once.
2. `git worktree add` makes sure a read-only base checkout exists for the chosen commit, then creates a session view on top of it and a branch `agent/<id>`.
3. `exec` runs a command (your agent, a shell, a build) inside that view.
4. Inside the view, `origin` is the real upstream, so `git push origin HEAD` pushes the session's branch. `drop` throws the session away.

A plain directory skips the git steps: `base add` snapshots it as the base, and `spawn` creates views of that snapshot.

tingly-fuse does not prepare environments (installing dependencies, building). Do that first in a normal checkout, then hand it over with `git worktree add --from-dir` (see below).

## Requirements

- Go 1.24+ to build, and `git` on `PATH`.
- **Linux**: as root the default `overlay` backend mounts overlayfs on the host. As non-root the default `fuse-overlay` backend needs `fuse-overlayfs` and `/dev/fuse`. The `overlay-ns` backend (a view only commands run by tingly-fuse can see) needs `nsenter` (util-linux), and as non-root Linux 5.11+ with unprivileged user namespaces.
- **macOS** (default `clone` backend): an APFS volume for copy-on-write. Other volumes fall back to a full copy.
- Anywhere else, or as a fallback, use `--backend copy`.

## Install

```sh
go install github.com/tingly-dev/tingly-fuse/cmd/tingly-fuse@latest
# or, from a clone of this repo
go build -o tingly-fuse ./cmd/tingly-fuse
```

## Quick start

```sh
# 1. Register a repository (mirrors it once).
tingly-fuse git clone https://github.com/tingly-dev/tingly-box

# 2. Start a session from a commit. Prints the session id; view path and branch go to stderr.
id=$(tingly-fuse git worktree add tingly-box main)

# 3. Run your agent inside the session's view.
tingly-fuse exec "$id" -- claude

# 4. Push the session's branch (agent/<id>) to the upstream, then open a PR there.
tingly-fuse exec "$id" -- git push origin HEAD

# 5. Remove the session when you are done. Unpushed work is lost.
tingly-fuse drop "$id"
```

Run as many sessions in parallel as you like. They all start from the same base and never see each other's changes:

```sh
for task in fix-login add-tests update-docs; do
  id=$(tingly-fuse git worktree add tingly-box main -b "agent/$task")
  tingly-fuse exec "$id" -- claude -p "$(cat tasks/$task.md)" &
done
wait
tingly-fuse list
```

### Start from a prepared directory

If your project needs installed dependencies or build output, prepare a normal checkout first, then snapshot it as the base:

```sh
cd ~/src/tingly-box && pnpm install && go build ./...    # your own setup, outside tingly-fuse

id=$(tingly-fuse git worktree add tingly-box --from-dir ~/src/tingly-box)
tingly-fuse list                                         # BASE column shows the new base id: dir-…
tingly-fuse git worktree add tingly-box --base dir-…     # more sessions reuse that snapshot, no new copy
```

- The snapshot is a reflink copy, ignored files such as `node_modules` included. Later changes to the source directory do not affect existing sessions.
- Unpushed commits, staged files and submodule objects in the directory are merged into the shared mirror, so nothing is lost.
- The directory's `.git` must be a directory. Worktrees are not supported.
- **pnpm on Linux:** inside an overlay view, pnpm sees a different device and falls back to a project-local store, reinstalling everything (about 690 MB on tingly-box). Set `npm_config_store_dir` in the session to the store you used when preparing; `pnpm install` then takes about 1.2 s and about 12 MB. Details in [ADR 0007](.design/decisions/0007-from-dir-snapshot-and-absorb-objects.md).

### Any directory

Without git, a directory is snapshotted as it is and each session gets a private view of the snapshot:

```sh
tingly-fuse base add data ~/datasets/eval --share cache  # prints the snapshot id: snap-…
id=$(tingly-fuse spawn data)
tingly-fuse exec "$id" -- ./run.sh                       # writes stay in the session, except under cache/
tingly-fuse base add data ~/datasets/eval                # a new snapshot; sessions spawned from now on use it
```

Nothing in the directory is interpreted: a `.git` in it is copied like any other file, so each session gets its own, and shared paths are not added to git's excludes.

## Commands

Any directory:

| Command | What it does |
|---|---|
| `base add <name> <dir> [--share path]...` | Snapshot `dir` as the base new sessions of `name` start from, registering `name` the first time (`--share` only then). Prints the snapshot id |
| `spawn <name> [--base id] [--id id] [--backend name] [--share path]...` | Create a session from the latest snapshot, or from `--base`, and print its id |

Git repositories. Each subcommand is named after the git command it corresponds to and takes its arguments in the same order, so a session is a worktree. There are only the three that a git command run inside a session cannot replace; push, fetch and the rest are plain git in the session (`exec <id> -- git push origin HEAD`), and `list` and `drop` work on git sessions ([ADR 0013](.design/decisions/0013-generic-layer-and-git-subcommand.md)):

| Command | Like | What it does |
|---|---|---|
| `git clone <upstream> [<name>] [--shared-git] [--share path]...` | `git clone` | Register a repository and create its shared mirror; the name defaults to the last part of `upstream`, as `git clone` names its directory; `--share` sets shared paths right away (repeatable or comma-separated, same as `share`); `--shared-git` makes every session a worktree of one shared repository, so branches and commits are visible to all sessions ([ADR 0010](.design/decisions/0010-shared-git-via-registered-worktrees.md)) |
| `git worktree add <name> [<commit-ish>] [flags]` | `git worktree add` | Create a session on its own branch and print its id |
| `git fsck <name> [--fix]` | `git fsck` | Check that every base is pinned against gc and still has all its objects; `--fix` pins old bases |

Either kind:

| Command | What it does |
|---|---|
| `exec <session> -- <cmd> [args...]` | Run a command inside the session's view |
| `checkpoint <session>` | Freeze the session's tree (uncommitted and untracked files included) as a new base and print its id; start sessions from it with `git worktree add --base` or `spawn --base`. Copies the whole tree, with reflinks where the filesystem has them; stop writers in the session first ([ADR 0017](.design/decisions/0017-checkpoint-and-fork.md)) |
| `fork <session> [--id id] [-b branch] [--backend name] [--share path]...` | `checkpoint`, then start a new session from it: it sees the parent's tree as it is now, and then the two diverge. The backend defaults to the parent's |
| `drop <session>` | Delete the session and its view |
| `list` | List sessions with repo, branch, base, backend and view path |
| `share <name> [path...]` / `unshare <name> <path>...` | Share directories or files across all new sessions, such as caches or `.env`: one copy on the host, bind mounted under overlay and symlinked under copy and clone; no path lists them ([ADR 0011](.design/decisions/0011-shared-paths.md)) |
| `base list [name]` | Without a name, list repositories with their kind, session and base counts, and source. With one, list its bases: creation time, the sessions using them, and the head commit and pin state (git) or which one is current (directory) |
| `base rm <name> <id>` | Delete a base no session uses (`id` may be a unique prefix); git gc then reclaims objects only it needed. A directory's current snapshot is kept |
| `base prune <name> [--dry-run]` | Delete every base no session uses, such as old snapshots |
| `remove <name> [--force]` | Delete the repository with all its bases (and mirror); `--force` drops its sessions first |

The commands before this split (`repo add`, `spawn`, `push`, `repo check`, `repo share`, …), and `git push`, `git fetch` and `git worktree list/remove`, now fail with a message naming their new form.

`git worktree add` arguments (flags may come before or after the positional ones, as in git):

| Argument | Meaning |
|---|---|
| `<commit-ish>` | Where to start (default: upstream `HEAD`). Anything but a full commit id is fetched first, so `main` means upstream's main now |
| `-b <branch>` | Branch to create (default `agent/<id>`) |
| `--no-fetch` | Resolve the commit-ish against the mirror as it is (offline, or to keep using the commit already there) |
| `--from-dir <dir>` | Snapshot a prepared checkout as the base, instead of a commit |
| `--base <id>` | Reuse an existing base (the id `git worktree add` prints as `base:`, or the BASE column of `list`) |
| `--id <id>` | Session id (default: random) |
| `--backend <name>` | `overlay`, `fuse-overlay`, `overlay-ns`, `clone` or `copy` (default depends on the platform and user) |
| `--share <path>` | Also share this path, on top of the repository's shared paths, with every other session spawned with it (repeatable or comma-separated) |

`spawn` takes `--base`, `--id`, `--backend` and `--share` with the same meaning. If either fails partway, it cleans up everything it created.

## Go API

Everything the command does is callable from Go through the package at the module root; the command itself uses nothing else ([ADR 0012](.design/decisions/0012-public-go-api.md)). It has the same two layers ([ADR 0013](.design/decisions/0013-generic-layer-and-git-subcommand.md)):

```go
import tinglyfuse "github.com/tingly-dev/tingly-fuse"

m, _ := tinglyfuse.Open("") // TINGLY_FUSE_HOME, or ~/.tingly-fuse

// Any directory.
m.AddDir(ctx, "data", "/srv/data", tinglyfuse.DirOptions{SharedPaths: []string{"cache"}})
s, _ := m.Spawn(ctx, tinglyfuse.SpawnOptions{Repo: "data"})

// Git repositories: m.Git().
m.Git().Clone(ctx, "https://github.com/tingly-dev/tingly-box", "box",
	tinglyfuse.AddOptions{SharedGit: true, SharedPaths: []string{".cache"}})
s, _ = m.Git().AddWorktree(ctx, tinglyfuse.WorktreeOptions{Repo: "box", Commit: "main", Share: []string{"tmp"}})
m.Exec(ctx, s.ID, []string{"go", "test", "./..."}, nil, os.Stdout, os.Stderr)
m.Exec(ctx, s.ID, []string{"git", "push", "origin", "HEAD"}, nil, os.Stdout, os.Stderr)
m.Drop(ctx, s.ID)
```

`tinglyfuse.CheckBackend(ctx, "overlay")` tells whether a backend can run on this host.

## Environment variables

Inside a session, every process started by `exec` gets:

| Variable | Value |
|---|---|
| `TINGLY_WORKSPACE` | The view path, which is also the working directory |
| `TINGLY_SESSION` | The session id |
| `TINGLY_REPO` | The repository name |

Scripts and agent configs should read `TINGLY_WORKSPACE` rather than hard-code a path, because on macOS each session has a different path.

Configuration:

| Variable | Default | Meaning |
|---|---|---|
| `TINGLY_FUSE_HOME` | `~/.tingly-fuse` | State directory |

## Backends

A backend only decides where a session's file tree lives and how to start a process that sees it. Git and session handling are shared.

| Backend | Platform | View path | How |
|---|---|---|---|
| `overlay` | Linux, root (default for root) | Different per session | Kernel overlayfs mounted on the host at the view path (lower = base, upper = session). Visible to everything on the machine: shells, editors, IDEs ([ADR 0015](.design/decisions/0015-host-visible-overlay-view.md)). |
| `fuse-overlay` | Linux, any user (default for non-root) | Different per session | `fuse-overlayfs` mounted on the host at the view path; visible like `overlay`, goes through FUSE (builds up to about 25% slower, scanning the tree about 5x slower: [bench/hostview.md](bench/hostview.md)). Shared paths are symlinks. |
| `overlay-ns` | Linux | Different per session | One long-lived mount namespace per session with overlayfs. Invisible to the host: on the host the view path is an empty mount point; works without root or FUSE. |
| `clone` | macOS (default) | Different per session | APFS `clonefile` copy-on-write; full copy on non-APFS volumes |
| `copy` | Any | Different per session | Full copy of the base; the reference implementation and fallback |

`overlay` and `fuse-overlay` mounts outlive the tingly-fuse process. After a reboot, or if the mount is lost, the next `exec` mounts the session's upper layer again without losing writes; `drop` unmounts, and fails (keeping the session) if a process is still using the view. With `overlay-ns`, concurrent `exec` calls join the same mount, and the next `exec` after the holder process dies remounts.

## State directory

```
~/.tingly-fuse/
├── repos/<name>/
│   ├── repo.json              registration (kind; upstream, or source directory and current snapshot)
│   ├── bases/<id>/            read-only bases: snap-… for a directory; for git, a commit, or dir-… for --from-dir
│   ├── shares/<path>          the one copy of each shared path
│   ├── objects.git            git only: bare mirror; the only place objects are stored
│   └── modules/<hash>.git     git only: submodule mirrors, shared across bases
└── sessions/<id>/
    ├── session.json           repo, base, branch, backend, view path
    └── ...                    backend state (overlay upper/work; overlay-ns holder.json)
```

## Limitations

- Each session has a different path. Scripts should read `TINGLY_WORKSPACE`; path-keyed build caches need options such as `go build -trimpath`.
- An `overlay-ns` view is only visible inside its session: on the host, its path is an empty mount point. Use `exec` to look inside, or use `overlay` or `fuse-overlay`.
- `overlay` and `fuse-overlay` views are real directories on the host that you and an agent can both write; tingly-fuse does not coordinate that.
- Sessions are fully isolated. Conflicts between sessions show up only after pushing, in the PR.
- `drop` deletes unpushed work.

See the [roadmap](.design/roadmap.md) for what is done and what is being considered.

## Development

```sh
go test -race ./...                 # unit, scenario and CLI tests
bench/lifecycle.sh                  # end to end on real tingly-box (needs network)
```

The tests are the harness that keeps the storage layer honest:

| Layer | Where | What it checks |
|---|---|---|
| Scenarios | `internal/manager/lifecycle_test.go` | Upstream rewrites, `--from-dir` objects, submodules, fetch freshness, pin repair, prune and prune racing spawn. Each one runs on every backend usable on the host (copy and the overlay backends on Linux, copy and clone on macOS) and ends with `git fsck` in the views |
| CLI | `cmd/tingly-fuse/main_test.go` | Every command through the real binary: arguments, output, exit codes |
| End to end | `bench/lifecycle.sh` | The same lifecycle on a real repository; `TF_OLD=<binary>` checks upgrading from an older version. Results in [bench/README.md](bench/README.md) |
| CI | `.github/workflows/ci.yml` | gofmt, vet, the tests on Linux (also as root, so overlay never skips) and macOS, and the end-to-end script on copy and overlay |

Design records live in [.design/](.design/): [architecture](.design/architecture.md), [roadmap](.design/roadmap.md), [decisions (ADRs)](.design/decisions/) and the [initial research](.design/research/research-multi-agent-git-workspace.md). Update them in the same PR when you change the design. Changes go through a branch and a pull request.

### Writing a backend

The interface is in [internal/backend/backend.go](internal/backend/backend.go).

- `Create` may only read `BaseDir`, never write it. All writes go under `StateDir`.
- The process returned by `Command` must have the view root as its working directory. Do not override variables already in `cmd.Env`; the manager appends after them.
- `Remove` must handle a half-created workspace.
- Call `backend.Register` in `init`, add a blank import in `cmd/tingly-fuse/main.go`, and set platform defaults through `backend.Default`.
