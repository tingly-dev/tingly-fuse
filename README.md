# tingly-fuse

**English** | [简体中文](README.zh-CN.md)

Run many coding agents on one git repository at the same time, without a worktree per agent and without container orchestration.

Each agent gets a **private, copy-on-write view** of the repository. It starts from a commit you choose, commits only to its own branch, and pushes that branch to your remote. Merging and review happen there, as normal pull requests.

## Why

When several agents share one checkout, they overwrite each other's files, index and `HEAD`. The usual fixes have costs:

- **A worktree or clone per agent** gives every agent a different path. Build caches that key on absolute paths (Go's, for example) miss on every new path, so each agent pays for a cold build. On [tingly-box](https://github.com/tingly-dev/tingly-box) that is about 150 s per agent.
- **A container per agent** solves isolation but brings images, orchestration and volume management along with it.

tingly-fuse separates what can be shared from what must be private:

| Shared by all sessions | Private to each session |
|---|---|
| Git objects (one bare mirror per repo) | Working-tree changes |
| Unchanged files of the base checkout | Index, `HEAD`, branch |
| Build caches, when the path is the same | Anything the agent writes |

On Linux, every session sees the repository at **the same path** (`/workspace/<repo>` by default), each through its own mount namespace. Path-keyed caches keep hitting, so from the second session on, the tingly-box `go build` drops from about 150 s to about 15 s. Creating a session takes about 30 ms and roughly 100 KB of disk. See the [benchmarks](bench/README.md).

## How it works

```
                 ~/.tingly-fuse/repos/<repo>/
                 ├── objects.git      one bare mirror; every git object lives here
                 └── bases/<commit>/  read-only checkout of a commit (.git uses alternates)
                            │
          ┌─────────────────┼─────────────────┐
          ▼                 ▼                 ▼
      session A         session B         session C        copy-on-write views
  /workspace/<repo>  /workspace/<repo>  /workspace/<repo>  (same path on Linux)
   branch agent/A     branch agent/B     branch agent/C
          │                 │                 │
          └──────── git push to upstream ─────┘  →  merge via PR on the remote
```

1. `repo add` registers an upstream and mirrors it once.
2. `spawn` makes sure a read-only base checkout exists for the chosen commit, then creates a session view on top of it and a branch `agent/<id>`.
3. `exec` runs a command (your agent, a shell, a build) inside that view.
4. `push` pushes the session's branch to the real upstream. `drop` throws the session away.

tingly-fuse does not prepare environments (installing dependencies, building). Do that first in a normal checkout, then hand it over with `spawn --from-dir` (see below).

## Requirements

- Go 1.24+ to build, and `git` on `PATH`.
- **Linux** (default `overlay` backend): overlayfs and `nsenter` (util-linux). As non-root you also need Linux 5.11+ with unprivileged user namespaces enabled, and a writable view root (see [Linux setup](#linux-setup)).
- **macOS** (default `clone` backend): an APFS volume for copy-on-write. Other volumes fall back to a full copy.
- Anywhere else, or as a fallback, use `--backend copy`.

## Install

```sh
go install github.com/tingly-dev/tingly-fuse/cmd/tingly-fuse@latest
# or, from a clone of this repo
go build -o tingly-fuse ./cmd/tingly-fuse
```

### Linux setup

As a non-root user, create the view root once:

```sh
sudo mkdir -p /workspace && sudo chown "$USER" /workspace
```

Or point it somewhere you already own with `TINGLY_FUSE_VIEW_ROOT=/path/to/views`.

## Quick start

```sh
# 1. Register a repository (mirrors it once).
tingly-fuse repo add tingly-box https://github.com/tingly-dev/tingly-box

# 2. Start a session from a commit. Prints the session id; view path and branch go to stderr.
id=$(tingly-fuse spawn tingly-box --from main)

# 3. Run your agent inside the session's view.
tingly-fuse exec "$id" -- claude

# 4. Push the session's branch (agent/<id>) to the upstream, then open a PR there.
tingly-fuse push "$id"

# 5. Remove the session when you are done. Unpushed work is lost.
tingly-fuse drop "$id"
```

Run as many sessions in parallel as you like. They all start from the same base and never see each other's changes:

```sh
for task in fix-login add-tests update-docs; do
  id=$(tingly-fuse spawn tingly-box --from main --branch "agent/$task")
  tingly-fuse exec "$id" -- claude -p "$(cat tasks/$task.md)" &
done
wait
tingly-fuse list
```

### Start from a prepared directory

If your project needs installed dependencies or build output, prepare a normal checkout first, then snapshot it as the base:

```sh
cd ~/src/tingly-box && pnpm install && go build ./...    # your own setup, outside tingly-fuse

id=$(tingly-fuse spawn tingly-box --from-dir ~/src/tingly-box)
tingly-fuse list                                         # BASE column shows the new base id: dir-…
tingly-fuse spawn tingly-box --base dir-…                # more sessions reuse that snapshot, no new copy
```

- The snapshot is a reflink copy, ignored files such as `node_modules` included. Later changes to the source directory do not affect existing sessions.
- Unpushed commits, staged files and submodule objects in the directory are merged into the shared mirror, so nothing is lost.
- The directory's `.git` must be a directory. Worktrees are not supported.
- **pnpm on Linux:** inside an overlay view, pnpm sees a different device and falls back to a project-local store, reinstalling everything (about 690 MB on tingly-box). Set `npm_config_store_dir` in the session to the store you used when preparing; `pnpm install` then takes about 1.2 s and about 12 MB. Details in [ADR 0007](.design/decisions/0007-from-dir-snapshot-and-absorb-objects.md).

## Commands

| Command | What it does |
|---|---|
| `repo add <name> <upstream> [--shared-git]` | Register a repository and create its shared mirror; `--shared-git` makes every session a worktree of one shared repository, so branches and commits are visible to all sessions ([ADR 0010](.design/decisions/0010-shared-git-via-registered-worktrees.md)) |
| `repo list [-v]` | List repositories; `-v` adds session and base counts |
| `repo fetch <name>` | Update the mirror from upstream |
| `repo check <name> [--fix]` | Check that every base is pinned against gc and still has all its objects; `--fix` pins old bases |
| `repo share <name> [path...]` / `repo unshare <name> <path>...` | Share directories or files across all new sessions, such as caches or `.env`: one copy on the host, bind mounted under overlay and symlinked under copy and clone; no path lists them ([ADR 0011](.design/decisions/0011-shared-paths.md)) |
| `base list <repo>` | List bases with their head commit, creation time, pin state and the sessions using them |
| `base rm <repo> <id>` | Delete a base no session uses (`id` may be a unique prefix); git gc then reclaims objects only it needed |
| `base prune <repo> [--dry-run]` | Delete every base no session uses, such as old `--from-dir` snapshots |
| `repo remove <name> [--force]` | Delete the mirror and all bases; `--force` drops the repo's sessions first |
| `spawn <repo> [flags]` | Create a session and print its id |
| `exec <session> -- <cmd> [args...]` | Run a command inside the session's view |
| `push <session> [--remote r]` | Push the session's branch (default remote `origin`, which is the real upstream) |
| `drop <session>` | Delete the session and its view |
| `list` | List sessions with repo, branch, base, backend and view path |

`spawn` flags:

| Flag | Meaning |
|---|---|
| `--from <rev>` | Revision to start from (default: upstream `HEAD`). Anything but a full commit id is fetched first, so `main` means upstream's main now |
| `--no-fetch` | Resolve `--from` against the mirror as it is (offline, or to keep using the commit already there) |
| `--from-dir <dir>` | Snapshot a prepared checkout as the base |
| `--base <id>` | Reuse an existing base (the id `spawn` prints as `base:`, or the BASE column of `list`) |
| `--branch <name>` | Branch to create (default `agent/<id>`) |
| `--id <id>` | Session id (default: random) |
| `--backend <name>` | `overlay`, `clone` or `copy` (default depends on the platform) |

If `spawn` fails partway, it cleans up everything it created.

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
| `TINGLY_FUSE_VIEW_ROOT` | `/workspace` | Linux overlay view root; read at `spawn` and stored with the session |

## Backends

A backend only decides where a session's file tree lives and how to start a process that sees it. Git and session handling are shared.

| Backend | Platform | View path | How |
|---|---|---|---|
| `overlay` | Linux (default) | Same for every session, `/workspace/<repo>` | One long-lived mount namespace per session with overlayfs (lower = base, upper = session). Invisible to the host and to other sessions. |
| `clone` | macOS (default) | Different per session | APFS `clonefile` copy-on-write; full copy on non-APFS volumes |
| `copy` | Any | Different per session | Full copy of the base; the reference implementation and fallback |

Concurrent `exec` calls on one overlay session join the same mount, so they see each other's writes. If the holder process dies, the next `exec` remounts from the session's upper layer without losing writes.

## State directory

```
~/.tingly-fuse/
├── repos/<name>/
│   ├── repo.json              registration (upstream)
│   ├── objects.git            bare mirror; the only place objects are stored
│   ├── modules/<hash>.git     submodule mirrors, shared across bases
│   └── bases/<commit>/        read-only base checkouts (dir-… for --from-dir snapshots)
└── sessions/<id>/
    ├── session.json           repo, base, branch, backend, view path
    └── ...                    backend state (overlay upper/work, holder.json)
```

## Limitations

- macOS has no mount namespaces, so sessions there get different paths. Mitigate with `TINGLY_WORKSPACE` in scripts and `go build -trimpath`. A same-path mode through a lightweight Linux VM is an idea, not implemented.
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
| Scenarios | `internal/manager/lifecycle_test.go` | Upstream rewrites, `--from-dir` objects, submodules, fetch freshness, pin repair, prune and prune racing spawn. Each one runs on every backend usable on the host (copy and overlay on Linux, copy and clone on macOS) and ends with `git fsck` in the views |
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
