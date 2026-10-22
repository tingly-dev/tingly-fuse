# tingly-fuse

[English](README.md) | **简体中文**

让多个编码 agent 同时使用同一个 git 仓库，不必为每个 agent 开 worktree，也不需要容器编排。

每个 agent 拿到仓库的一个**私有的、写时复制的视图**：从你指定的 commit 出发，只往自己的分支上提交，最后把分支推到远端。合并和评审在远端以普通 PR 的方式进行。

视图也能用在任意目录上，不管有没有 Git：`base add` 把目录快照下来，`spawn` 给每个 agent 一份。镜像、commit、分支、推送、共享 `.git` 这些 Git 功能都在 `tingly-fuse git` 下面（[ADR 0013](.design/decisions/0013-generic-layer-and-git-subcommand.md)）。

## 为什么

多个 agent 共用一个 checkout 时，会互相覆盖文件、index 和 `HEAD`。常见的解决办法都有代价：

- **每个 agent 一个 worktree 或 clone**：每个 agent 的路径都不同。按绝对路径命中的构建缓存（比如 Go 的）每换一个路径就失效，每个 agent 都要冷构建一次。在 [tingly-box](https://github.com/tingly-dev/tingly-box) 上每次约 150 秒。
- **每个 agent 一个容器**：隔离解决了，但随之而来的是镜像、编排和卷管理。

tingly-fuse 把能共享的和必须私有的分开：

| 所有会话共享 | 每个会话私有 |
|---|---|
| git 对象（每个仓库一个 bare 镜像） | 工作区改动 |
| 基础 checkout 中未改动的文件 | index、`HEAD`、分支 |
| 不按路径区分的构建缓存 | agent 写入的一切 |

所有平台上每个会话都有自己的路径：`~/.tingly-fuse/sessions/<id>/root`。按绝对路径命中的构建缓存要用工具自己的选项绕开：共用 `GOCACHE` 并加 `go build -trimpath` 时，第二个会话构建 tingly-box 约 10 秒，不加约 90 秒。在 Linux 上新建一个会话约 30 毫秒、约 100 KB 磁盘。详见[基准测试](bench/README.md)和 [ADR 0014](.design/decisions/0014-per-session-view-path.md)。

## 工作原理

```
                 ~/.tingly-fuse/repos/<repo>/
                 ├── objects.git      唯一的 bare 镜像，所有 git 对象都在这里
                 └── bases/<commit>/  某个 commit 的只读 checkout（.git 用 alternates）
                            │
          ┌─────────────────┼─────────────────┐
          ▼                 ▼                 ▼
       会话 A            会话 B            会话 C          写时复制的视图
 sessions/A/root    sessions/B/root    sessions/C/root    （每个会话一个路径）
   分支 agent/A       分支 agent/B       分支 agent/C
          │                 │                 │
          └────────── git push 到 upstream ───┘  →  在远端通过 PR 合并
```

1. `git clone` 登记一个 upstream，并镜像一次。
2. `git worktree add` 确保所选 commit 的只读基础 checkout 存在，在其上建出会话视图和分支 `agent/<id>`。
3. `exec` 在这个视图里运行命令（你的 agent、shell、构建）。
4. 视图里的 `origin` 就是真实的 upstream，`git push origin HEAD` 就把会话分支推上去；`drop` 丢弃会话。

普通目录跳过 Git 这几步：`base add` 把它快照成 base，`spawn` 在这份快照上建视图。

tingly-fuse 不负责准备环境（装依赖、构建）。先在普通 checkout 里准备好，再用 `git worktree add --from-dir` 交给它（见下文）。

## 环境要求

- 构建需要 Go 1.24+，`PATH` 里要有 `git`。
- **Linux**（默认 `overlay` 后端）：overlayfs 和 `nsenter`（util-linux）。非 root 用户还需要 Linux 5.11+、允许非特权 user namespace。
- **macOS**（默认 `clone` 后端）：APFS 卷才能写时复制，其他卷自动退回整份复制。
- 其他平台或作为兜底，用 `--backend copy`。

## 安装

```sh
go install github.com/tingly-dev/tingly-fuse/cmd/tingly-fuse@latest
# 或者在本仓库的 clone 里
go build -o tingly-fuse ./cmd/tingly-fuse
```

## 快速上手

```sh
# 1. 登记仓库（镜像一次）
tingly-fuse git clone https://github.com/tingly-dev/tingly-box

# 2. 从某个 commit 开一个会话。stdout 打印会话 id，视图路径和分支打印在 stderr
id=$(tingly-fuse git worktree add tingly-box main)

# 3. 在会话视图里运行 agent
tingly-fuse exec "$id" -- claude

# 4. 把会话分支（agent/<id>）推到 upstream，然后在远端开 PR
tingly-fuse exec "$id" -- git push origin HEAD

# 5. 用完删除会话。未推送的工作会丢失
tingly-fuse drop "$id"
```

想并行多少个会话都可以。它们从同一个 base 出发，彼此看不到对方的改动：

```sh
for task in fix-login add-tests update-docs; do
  id=$(tingly-fuse git worktree add tingly-box main -b "agent/$task")
  tingly-fuse exec "$id" -- claude -p "$(cat tasks/$task.md)" &
done
wait
tingly-fuse list
```

### 从准备好的目录开始

如果项目需要装好的依赖或构建产物，先准备一个普通 checkout，再把它快照成 base：

```sh
cd ~/src/tingly-box && pnpm install && go build ./...    # 你自己的准备步骤，在 tingly-fuse 之外

id=$(tingly-fuse git worktree add tingly-box --from-dir ~/src/tingly-box)
tingly-fuse list                                         # BASE 列显示新 base 的 id：dir-…
tingly-fuse git worktree add tingly-box --base dir-…     # 之后的会话复用这份快照，不再复制
```

- 快照用 reflink 复制，包括 `node_modules` 等被忽略的文件。之后源目录怎么改都不影响已有会话。
- 目录里未推送的提交、已暂存的文件和子模块对象会并入共享镜像，不会丢。
- 目录的 `.git` 必须是目录，不支持 worktree。
- **Linux 上的 pnpm**：在 overlay 视图里 pnpm 会发现设备号不同，改用项目内 store 并整份重装（tingly-box 上约 690 MB）。在会话里把 `npm_config_store_dir` 设成准备环境时用的 store，`pnpm install` 就只要约 1.2 秒、约 12 MB。细节见 [决策 0007](.design/decisions/0007-from-dir-snapshot-and-absorb-objects.md)。

### 任意目录

不用 Git 时，目录按原样快照下来，每个会话拿到这份快照的私有视图：

```sh
tingly-fuse base add data ~/datasets/eval --share cache  # 打印快照 id：snap-…
id=$(tingly-fuse spawn data)
tingly-fuse exec "$id" -- ./run.sh                       # 写入只留在会话里，cache/ 下除外
tingly-fuse base add data ~/datasets/eval                # 再拍一个快照，之后新开的会话用它
```

目录里的内容不做任何解释：里面的 `.git` 和普通文件一样被复制，每个会话一份；共享路径也不会写进 Git 的 exclude。

## 命令

任意目录：

| 命令 | 做什么 |
|---|---|
| `base add <name> <dir> [--share path]...` | 把 `dir` 快照成 `name` 新会话的起点；第一次运行时登记 `name`（`--share` 只能在这时给）。打印快照 id |
| `spawn <name> [--base id] [--id id] [--backend name] [--share path]...` | 从最新快照（或 `--base` 指定的 base）新建会话并打印 id |

Git 仓库。每个子命令都以对应的 git 命令命名，参数顺序也一样，会话就是一个 worktree。只有这三个是在会话里跑 git 替代不了的；推送、拉取等直接在会话里跑 git（`exec <id> -- git push origin HEAD`），列出和删除会话用 `list`、`drop`（[ADR 0013](.design/decisions/0013-generic-layer-and-git-subcommand.md)）：

| 命令 | 对应 | 做什么 |
|---|---|---|
| `git clone <upstream> [<name>] [--shared-git] [--share path]...` | `git clone` | 登记仓库并建共享镜像；不给名字时取 `upstream` 的最后一段，和 `git clone` 给目录起名一样；`--share` 同时设置共享路径（可重复，也可逗号分隔，与 `share` 相同）；`--shared-git` 让所有会话成为同一个共享仓库的 worktree，分支和提交彼此立即可见（[ADR 0010](.design/decisions/0010-shared-git-via-registered-worktrees.md)） |
| `git worktree add <name> [<commit-ish>] [flags]` | `git worktree add` | 新建会话（带自己的分支）并打印 id |
| `git fsck <name> [--fix]` | `git fsck` | 检查每个 base 是否已固定（不会被 gc 清掉）、对象是否齐全；`--fix` 为旧 base 补上固定 |

两种都适用：

| 命令 | 做什么 |
|---|---|
| `exec <session> -- <cmd> [args...]` | 在会话视图里运行命令 |
| `drop <session>` | 删除会话和它的视图 |
| `list` | 列出会话：仓库、分支、base、后端、视图路径 |
| `share <name> [path...]` / `unshare <name> <path>...` | 让所有新会话共享指定的目录或文件，例如缓存或 `.env`：宿主上一份，overlay 下 bind mount，copy 和 clone 下用符号链接；不带路径时列出（[ADR 0011](.design/decisions/0011-shared-paths.md)） |
| `base list [name]` | 不带名字时列出所有仓库：类型、会话数、base 数、来源。带名字时列出它的 base：创建时间、哪些会话在用，以及所在 commit 和是否已固定（Git）或哪个是当前快照（目录） |
| `base rm <name> <id>` | 删除没有会话在用的 base（`id` 可以是唯一前缀）；之后 git gc 会回收只有它需要的对象。目录的当前快照不会被删 |
| `base prune <name> [--dry-run]` | 删除所有没有会话在用的 base，比如旧的快照 |
| `remove <name> [--force]` | 删除仓库和它所有的 base（以及镜像）；`--force` 先 drop 它的会话 |

拆分之前的命令（`repo add`、`spawn`、`push`、`repo check`、`repo share` 等），以及 `git push`、`git fetch`、`git worktree list/remove`，现在会报错，并给出新的写法。

`git worktree add` 的参数（和 git 一样，选项可以放在位置参数前面或后面）：

| 参数 | 含义 |
|---|---|
| `<commit-ish>` | 从哪里开始（默认 upstream 的 `HEAD`）。除完整 commit id 外都会先 fetch，所以 `main` 指的是 upstream 现在的 main |
| `-b <branch>` | 要创建的分支（默认 `agent/<id>`） |
| `--no-fetch` | 直接用镜像里现有的内容解析 commit-ish（离线，或想继续用已有的 commit） |
| `--from-dir <dir>` | 把准备好的 checkout 快照成 base，代替 commit |
| `--base <id>` | 复用已有 base（id 即 `git worktree add` 打印的 `base:`，或 `list` 的 BASE 列） |
| `--id <id>` | 会话 id（默认随机） |
| `--backend <name>` | `overlay`、`clone` 或 `copy`（默认值取决于平台） |
| `--share <path>` | 在仓库的共享路径之外，再和其他带同一路径 spawn 的会话共享这个路径（可重复，也可逗号分隔） |

`spawn` 也接受 `--base`、`--id`、`--backend`、`--share`，含义相同。两者中途失败都会清理掉已创建的一切。

## Go API

命令能做的一切都可以在 Go 里通过模块根目录的包调用；命令本身只用这个包（[ADR 0012](.design/decisions/0012-public-go-api.md)）。它也分同样的两层（[ADR 0013](.design/decisions/0013-generic-layer-and-git-subcommand.md)）：

```go
import tinglyfuse "github.com/tingly-dev/tingly-fuse"

m, _ := tinglyfuse.Open("") // TINGLY_FUSE_HOME，或 ~/.tingly-fuse

// 任意目录
m.AddDir(ctx, "data", "/srv/data", tinglyfuse.DirOptions{SharedPaths: []string{"cache"}})
s, _ := m.Spawn(ctx, tinglyfuse.SpawnOptions{Repo: "data"})

// Git 仓库：m.Git()
m.Git().Clone(ctx, "https://github.com/tingly-dev/tingly-box", "box",
	tinglyfuse.AddOptions{SharedGit: true, SharedPaths: []string{".cache"}})
s, _ = m.Git().AddWorktree(ctx, tinglyfuse.WorktreeOptions{Repo: "box", Commit: "main", Share: []string{"tmp"}})
m.Exec(ctx, s.ID, []string{"go", "test", "./..."}, nil, os.Stdout, os.Stderr)
m.Exec(ctx, s.ID, []string{"git", "push", "origin", "HEAD"}, nil, os.Stdout, os.Stderr)
m.Drop(ctx, s.ID)
```

`tinglyfuse.CheckBackend(ctx, "overlay")` 判断某个后端能否在本机运行。

## 环境变量

会话里由 `exec` 启动的每个进程都会拿到：

| 变量 | 值 |
|---|---|
| `TINGLY_WORKSPACE` | 视图路径，也是工作目录 |
| `TINGLY_SESSION` | 会话 id |
| `TINGLY_REPO` | 仓库名 |

脚本和 agent 配置应读 `TINGLY_WORKSPACE`，不要写死路径，因为 macOS 上每个会话的路径都不同。

配置项：

| 变量 | 默认值 | 含义 |
|---|---|---|
| `TINGLY_FUSE_HOME` | `~/.tingly-fuse` | 状态目录 |

## 后端

后端只决定会话的文件树放在哪、怎么启动一个能看到它的进程。git 和会话管理是共用的。

| 后端 | 平台 | 视图路径 | 机制 |
|---|---|---|---|
| `overlay` | Linux（默认） | 每会话不同 | 每会话一个常驻 mount namespace，里面挂 overlayfs（lower = base，upper = 会话）。宿主和其他会话都看不到，宿主上视图路径是空的挂载点 |
| `clone` | macOS（默认） | 每会话不同 | APFS `clonefile` 写时复制；非 APFS 卷整份复制 |
| `copy` | 全平台 | 每会话不同 | 整份复制 base，作为基准实现和兜底 |

同一个 overlay 会话上的并发 `exec` 进入的是同一个挂载，能互相看到写入。holder 进程意外退出后，下一次 `exec` 会用会话的 upper 层重新挂载，写入不会丢。

## 状态目录

```
~/.tingly-fuse/
├── repos/<name>/
│   ├── repo.json              登记信息（类型；upstream，或源目录和当前快照）
│   ├── bases/<id>/            只读 base：目录为 snap-…；Git 为某个 commit，--from-dir 快照为 dir-…
│   ├── shares/<path>          每个共享路径在宿主上的唯一一份
│   ├── objects.git            仅 Git：bare 镜像，对象只存在这里
│   └── modules/<hash>.git     仅 Git：子模块镜像，跨 base 共用
└── sessions/<id>/
    ├── session.json           仓库、base、分支、后端、视图路径
    └── ...                    后端状态（overlay 的 upper/work、holder.json）
```

## 已知限制

- 每个会话路径不同。脚本应读 `TINGLY_WORKSPACE`；按路径命中的构建缓存要加 `go build -trimpath` 这类选项。
- overlay 的视图只在会话里可见，宿主上它的路径是空的挂载点，要看里面用 `exec`。
- 会话之间完全隔离，冲突只能在推送之后、在 PR 里发现。
- `drop` 会删除未推送的工作。

做了什么、在考虑什么，见[路线图](.design/roadmap.md)。

## 开发

```sh
go test -race ./...                 # 单元、场景和 CLI 测试
bench/lifecycle.sh                  # 在真实 tingly-box 上端到端（需要网络）
```

这些测试就是存储层的 harness：

| 层次 | 位置 | 检查什么 |
|---|---|---|
| 场景 | `internal/manager/lifecycle_test.go` | 上游改写历史、`--from-dir` 的对象、子模块、fetch 新鲜度、补固定、prune 以及 prune 与 spawn 并发。每个场景在本机能用的所有后端上跑（Linux 上 copy 和 overlay，macOS 上 copy 和 clone），最后在视图里 `git fsck` |
| CLI | `cmd/tingly-fuse/main_test.go` | 通过真实二进制跑每个命令：参数、输出、退出码 |
| 端到端 | `bench/lifecycle.sh` | 在真实仓库上跑同样的生命周期；`TF_OLD=<二进制>` 验证从旧版本升级。结果见 [bench/README.md](bench/README.md) |
| CI | `.github/workflows/ci.yml` | gofmt、vet；Linux（另以 root 跑一遍，overlay 不会被跳过）和 macOS 上的测试；copy 和 overlay 上的端到端脚本 |

设计记录在 [.design/](.design/)：[架构](.design/architecture.md)、[路线图](.design/roadmap.md)、[决策记录](.design/decisions/)和[前期调研](.design/research/research-multi-agent-git-workspace.md)。改动设计时请在同一个 PR 里同步更新。改动走分支加 PR。

### 实现新后端

接口见 [internal/backend/backend.go](internal/backend/backend.go)。

- `Create` 只能读 `BaseDir`，绝不能写；所有写入落在 `StateDir` 之下。
- `Command` 返回的进程工作目录必须是视图根目录；不要覆盖 `cmd.Env` 里已有的变量，manager 会在其后追加。
- `Remove` 必须能处理只创建了一半的工作区。
- 在 `init` 里调用 `backend.Register`，在 `cmd/tingly-fuse/main.go` 里匿名导入；平台默认值通过 `backend.Default` 设置。
