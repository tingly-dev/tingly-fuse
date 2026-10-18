# tingly-fuse

[English](README.md) | **简体中文**

让多个编码 agent 同时使用同一个 git 仓库，不必为每个 agent 开 worktree，也不需要容器编排。

每个 agent 拿到仓库的一个**私有的、写时复制的视图**：从你指定的 commit 出发，只往自己的分支上提交，最后把分支推到远端。合并和评审在远端以普通 PR 的方式进行。

## 为什么

多个 agent 共用一个 checkout 时，会互相覆盖文件、index 和 `HEAD`。常见的解决办法都有代价：

- **每个 agent 一个 worktree 或 clone**：每个 agent 的路径都不同。按绝对路径命中的构建缓存（比如 Go 的）每换一个路径就失效，每个 agent 都要冷构建一次。在 [tingly-box](https://github.com/tingly-dev/tingly-box) 上每次约 150 秒。
- **每个 agent 一个容器**：隔离解决了，但随之而来的是镜像、编排和卷管理。

tingly-fuse 把能共享的和必须私有的分开：

| 所有会话共享 | 每个会话私有 |
|---|---|
| git 对象（每个仓库一个 bare 镜像） | 工作区改动 |
| 基础 checkout 中未改动的文件 | index、`HEAD`、分支 |
| 构建缓存（路径相同时） | agent 写入的一切 |

在 Linux 上，所有会话都在**同一个路径**（默认 `/workspace/<repo>`）下看到仓库，各自通过自己的 mount namespace。按路径命中的缓存一直有效，从第二个会话起，tingly-box 的 `go build` 从约 150 秒降到约 15 秒。新建一个会话约 30 毫秒、约 100 KB 磁盘。详见[基准测试](bench/README.md)。

## 工作原理

```
                 ~/.tingly-fuse/repos/<repo>/
                 ├── objects.git      唯一的 bare 镜像，所有 git 对象都在这里
                 └── bases/<commit>/  某个 commit 的只读 checkout（.git 用 alternates）
                            │
          ┌─────────────────┼─────────────────┐
          ▼                 ▼                 ▼
       会话 A            会话 B            会话 C          写时复制的视图
  /workspace/<repo>  /workspace/<repo>  /workspace/<repo>  （Linux 上路径相同）
   分支 agent/A       分支 agent/B       分支 agent/C
          │                 │                 │
          └────────── git push 到 upstream ───┘  →  在远端通过 PR 合并
```

1. `repo add` 登记一个 upstream，并镜像一次。
2. `spawn` 确保所选 commit 的只读基础 checkout 存在，在其上建出会话视图和分支 `agent/<id>`。
3. `exec` 在这个视图里运行命令（你的 agent、shell、构建）。
4. `push` 把会话分支推到真实的 upstream；`drop` 丢弃会话。

tingly-fuse 不负责准备环境（装依赖、构建）。先在普通 checkout 里准备好，再用 `spawn --from-dir` 交给它（见下文）。

## 环境要求

- 构建需要 Go 1.24+，`PATH` 里要有 `git`。
- **Linux**（默认 `overlay` 后端）：overlayfs 和 `nsenter`（util-linux）。非 root 用户还需要 Linux 5.11+、允许非特权 user namespace，以及可写的视图根目录（见 [Linux 准备](#linux-准备)）。
- **macOS**（默认 `clone` 后端）：APFS 卷才能写时复制，其他卷自动退回整份复制。
- 其他平台或作为兜底，用 `--backend copy`。

## 安装

```sh
go install github.com/tingly-dev/tingly-fuse/cmd/tingly-fuse@latest
# 或者在本仓库的 clone 里
go build -o tingly-fuse ./cmd/tingly-fuse
```

### Linux 准备

非 root 用户先建一次视图根目录：

```sh
sudo mkdir -p /workspace && sudo chown "$USER" /workspace
```

或者用 `TINGLY_FUSE_VIEW_ROOT=/path/to/views` 指向一个你已有权限的目录。

## 快速上手

```sh
# 1. 登记仓库（镜像一次）
tingly-fuse repo add tingly-box https://github.com/tingly-dev/tingly-box

# 2. 从某个 commit 开一个会话。stdout 打印会话 id，视图路径和分支打印在 stderr
id=$(tingly-fuse spawn tingly-box --from main)

# 3. 在会话视图里运行 agent
tingly-fuse exec "$id" -- claude

# 4. 把会话分支（agent/<id>）推到 upstream，然后在远端开 PR
tingly-fuse push "$id"

# 5. 用完删除会话。未推送的工作会丢失
tingly-fuse drop "$id"
```

想并行多少个会话都可以。它们从同一个 base 出发，彼此看不到对方的改动：

```sh
for task in fix-login add-tests update-docs; do
  id=$(tingly-fuse spawn tingly-box --from main --branch "agent/$task")
  tingly-fuse exec "$id" -- claude -p "$(cat tasks/$task.md)" &
done
wait
tingly-fuse list
```

### 从准备好的目录开始

如果项目需要装好的依赖或构建产物，先准备一个普通 checkout，再把它快照成 base：

```sh
cd ~/src/tingly-box && pnpm install && go build ./...    # 你自己的准备步骤，在 tingly-fuse 之外

id=$(tingly-fuse spawn tingly-box --from-dir ~/src/tingly-box)
tingly-fuse list                                         # BASE 列显示新 base 的 id：dir-…
tingly-fuse spawn tingly-box --base dir-…                # 之后的会话复用这份快照，不再复制
```

- 快照用 reflink 复制，包括 `node_modules` 等被忽略的文件。之后源目录怎么改都不影响已有会话。
- 目录里未推送的提交、已暂存的文件和子模块对象会并入共享镜像，不会丢。
- 目录的 `.git` 必须是目录，不支持 worktree。
- **Linux 上的 pnpm**：在 overlay 视图里 pnpm 会发现设备号不同，改用项目内 store 并整份重装（tingly-box 上约 690 MB）。在会话里把 `npm_config_store_dir` 设成准备环境时用的 store，`pnpm install` 就只要约 1.2 秒、约 12 MB。细节见 [决策 0007](.design/decisions/0007-from-dir-snapshot-and-absorb-objects.md)。

## 命令

| 命令 | 做什么 |
|---|---|
| `repo add <name> <upstream>` | 登记仓库并建共享镜像 |
| `repo list [-v]` | 列出仓库；`-v` 加上会话数和 base 数 |
| `repo fetch <name>` | 从 upstream 更新镜像 |
| `repo check <name> [--fix]` | 检查每个 base 是否已固定（不会被 gc 清掉）、对象是否齐全；`--fix` 为旧 base 补上固定 |
| `base list <repo>` | 列出 base：所在 commit、创建时间、是否已固定、哪些会话在用 |
| `base rm <repo> <id>` | 删除没有会话在用的 base（`id` 可以是唯一前缀）；之后 git gc 会回收只有它需要的对象 |
| `base prune <repo> [--dry-run]` | 删除所有没有会话在用的 base，比如旧的 `--from-dir` 快照 |
| `repo remove <name> [--force]` | 删除镜像和所有 base；`--force` 先 drop 该仓库的会话 |
| `spawn <repo> [flags]` | 新建会话并打印 id |
| `exec <session> -- <cmd> [args...]` | 在会话视图里运行命令 |
| `push <session> [--remote r]` | 推送会话分支（默认 remote 是 `origin`，即真实 upstream） |
| `drop <session>` | 删除会话和它的视图 |
| `list` | 列出会话：仓库、分支、base、后端、视图路径 |

`spawn` 的参数：

| 参数 | 含义 |
|---|---|
| `--from <rev>` | 起始 revision（默认 upstream 的 `HEAD`）。除完整 commit id 外都会先 fetch，所以 `main` 指的是 upstream 现在的 main |
| `--no-fetch` | 直接用镜像里现有的内容解析 `--from`（离线，或想继续用已有的 commit） |
| `--from-dir <dir>` | 把准备好的 checkout 快照成 base |
| `--base <id>` | 复用已有 base（id 即 `spawn` 打印的 `base:`，或 `list` 的 BASE 列） |
| `--branch <name>` | 要创建的分支（默认 `agent/<id>`） |
| `--id <id>` | 会话 id（默认随机） |
| `--backend <name>` | `overlay`、`clone` 或 `copy`（默认值取决于平台） |

`spawn` 中途失败会清理掉已创建的一切。

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
| `TINGLY_FUSE_VIEW_ROOT` | `/workspace` | Linux overlay 的视图根目录；在 `spawn` 时读取并记入会话 |

## 后端

后端只决定会话的文件树放在哪、怎么启动一个能看到它的进程。git 和会话管理是共用的。

| 后端 | 平台 | 视图路径 | 机制 |
|---|---|---|---|
| `overlay` | Linux（默认） | 所有会话相同，`/workspace/<repo>` | 每会话一个常驻 mount namespace，里面挂 overlayfs（lower = base，upper = 会话）。宿主和其他会话都看不到 |
| `clone` | macOS（默认） | 每会话不同 | APFS `clonefile` 写时复制；非 APFS 卷整份复制 |
| `copy` | 全平台 | 每会话不同 | 整份复制 base，作为基准实现和兜底 |

同一个 overlay 会话上的并发 `exec` 进入的是同一个挂载，能互相看到写入。holder 进程意外退出后，下一次 `exec` 会用会话的 upper 层重新挂载，写入不会丢。

## 状态目录

```
~/.tingly-fuse/
├── repos/<name>/
│   ├── repo.json              登记信息（upstream）
│   ├── objects.git            bare 镜像，对象只存在这里
│   ├── modules/<hash>.git     子模块镜像，跨 base 共用
│   └── bases/<commit>/        只读基础 checkout（--from-dir 快照为 dir-…）
└── sessions/<id>/
    ├── session.json           仓库、base、分支、后端、视图路径
    └── ...                    后端状态（overlay 的 upper/work、holder.json）
```

## 已知限制

- macOS 没有 mount namespace，会话路径各不相同。缓解办法是脚本读 `TINGLY_WORKSPACE`、Go 构建加 `-trimpath`。用轻量 Linux 虚拟机实现同一路径只是设想，尚未实现。
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
