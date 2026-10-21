# 架构

## 目标

让多个 agent 同时使用同一个 git 仓库，不为每个 agent 开 worktree，也不做容器编排。每个 agent 得到一个私有的、写时复制的工作区视图，从某个 commit 出发，只在自己的分支上堆叠改动，推到远端后再处理合并。

## 核心原则

1. **共享存储，隔离工作空间。** 对象库和未改动的文件所有会话共享；工作区文件、index、HEAD 每会话一份。（[0003](decisions/0003-shared-objects-via-alternates.md)）指定的路径可以设为所有会话共享（[0011](decisions/0011-shared-paths.md)），`.git` 用 `--shared-git` 单独处理（[0010](decisions/0010-shared-git-via-registered-worktrees.md)）。
2. **路径一致。** 在支持的平台上，所有会话在同一个路径下工作，背后是各自的视图。Go 构建缓存按绝对路径命中，路径一致能让第二个会话起的构建从约 150 秒降到约 15 秒。（[0002](decisions/0002-same-path-private-view.md)）
3. **合并交给远端。** 会话只负责产出分支并推送，本地不做合并。（[0005](decisions/0005-merge-on-remote.md)）
4. **通用层和 Git 层分开。** 视图、共享路径、base 回收对任意目录都成立；镜像、按 commit 建 base、分支、推送、共享 `.git` 只属于 Git 仓库，命令在 `git` 子命令下，代码在 `internal/manager/git.go`（[0013](decisions/0013-generic-layer-and-git-subcommand.md)）。
5. **职责边界。** 环境准备（装依赖、构建）发生在 tingly-fuse 之前，不归它管。它只解决"拿到一个现成的底层之后，多个 agent 如何互不干扰"。（[0004](decisions/0004-environment-prep-out-of-scope.md)）

## 状态目录

默认 `~/.tingly-fuse`，可用 `TINGLY_FUSE_HOME` 覆盖。

```
repos/<name>/repo.json          登记信息：kind（dir 或 git；旧文件没有，按 git 读），git 的 upstream，或 dir 的源目录和当前快照
repos/<name>/bases/snap-<t>-<r>/  dir 仓库：base add 拍下的目录快照
repos/<name>/objects.git        bare 镜像，所有 git 对象只存在这里；refs/tingly/bases/<id>/ 下是各 base 的固定 ref
repos/<name>/modules/<h>.git    子模块的 bare 镜像，按 URL 哈希，跨 base 共用
repos/<name>/shares/<path>      每个共享路径在宿主上的唯一一份（share）
repos/<name>/shared.git         仅 --shared-git：所有会话共用的 refs、config 和新对象；worktrees/<id>/ 是各会话的 HEAD、index、子模块 gitdir
repos/<name>/bases/<sha>/       某个 commit 的只读基础 checkout
repos/<name>/bases/dir-<sha12>-<t>/  由 git spawn --from-dir 从现成目录快照出的基础 checkout
sessions/<id>/session.json      会话元数据（repo、base、分支、后端、视图路径）
sessions/<id>/...               后端私有状态（overlay 的 upper/work、holder.json 等）
```

基础 checkout 的 `.git` 以及它的子模块仓库都只带 alternates，指向上面的镜像，自己不存对象。原因见 [0003](decisions/0003-shared-objects-via-alternates.md)。每个 base 在它借用的每个镜像里都有固定 ref，镜像 gc 时不会删掉 base 还要用的对象（[0008](decisions/0008-pin-base-objects-in-mirrors.md)）。

## 命令与流程

通用层，任意目录（[0013](decisions/0013-generic-layer-and-git-subcommand.md)）：

| 命令 | 做什么 |
|---|---|
| `base add <name> <dir>` | 第一次运行时登记 dir 仓库（`--share` 只在这时可给）；每次都把目录 reflink 快照成新 base 并记为当前快照，内容不做解释 |
| `spawn <name> [--base id]` | 从当前快照（或指定 base）建视图；不建分支、不碰 Git |

Git 层：

| 命令 | 做什么 |
|---|---|
| `git add` | 登记仓库并建镜像（`--shared-git`：会话共享 refs 和新对象，见 [0010](decisions/0010-shared-git-via-registered-worktrees.md)） |
| `git spawn <name> [--from rev \| --from-dir dir \| --base id]` | `--from-dir`：reflink 快照该目录并把其中对象并入镜像（[0007](decisions/0007-from-dir-snapshot-and-absorb-objects.md)）；`--base`：复用已有 base；默认：解析 rev 得到 commit（除完整 commit id 外先 fetch，`--no-fetch` 跳过）；固定 base 的对象；确保该 commit 的 base 存在（并发安全，先建临时目录再 rename）；之后和通用层一样建视图，再在视图里接上共享 `.git`、把未跟踪的共享路径写进 `info/exclude`、建分支 `agent/<id>` |
| `git push <session>` | 在视图里把分支推到真实的 upstream（base 的 origin 指向 upstream，不是本地镜像） |
| `git fetch` / `git check [--fix]` | 更新镜像；逐个 base 检查是否已固定、`git fsck` 是否通过（`--fix` 给旧 base 补固定）（[0008](decisions/0008-pin-base-objects-in-mirrors.md)） |

两种仓库都适用：

| 命令 | 做什么 |
|---|---|
| `exec <session> -- cmd...` | 在会话视图里运行命令，注入 `TINGLY_WORKSPACE`、`TINGLY_SESSION`、`TINGLY_REPO` |
| `drop <session>` | 后端 `Remove` 并删除会话目录，未推送的工作随之丢失（共享 `.git` 时分支和提交留在 shared.git） |
| `list` | 列出会话 |
| `share/unshare` | 设置所有新会话共享的目录或文件；不带路径时列出；`git add --share`、`base add --share`、`spawn --share`、`git spawn --share` 可在创建时一并设置（[0011](decisions/0011-shared-paths.md)） |
| `base list [name]` / `base rm` / `base prune` | 不带名字时列出仓库；带名字时列出 base 及引用它的会话；删除没有会话引用的 base，连同它在镜像里的固定 ref；dir 仓库的当前快照不删；与 spawn 用 `.bases.lock` 互斥（[0009](decisions/0009-base-removal-by-session-refcount.md)） |
| `remove [--force]` | 删除仓库（`--force` 先 drop 它的会话） |

两种 spawn 中途失败都会回收已创建的一切（视图、共享 `.git` 的 worktree 条目、会话目录），不留半成品。

## Go API

模块根目录的 `tinglyfuse` 包是对外接口，类型来自 `internal/manager` 等包的别名；命令行只通过它调用（[0012](decisions/0012-public-go-api.md)）。和命令一样分两层：`Manager` 上是通用方法，`Manager.Git()` 上是 Git 方法（[0013](decisions/0013-generic-layer-and-git-subcommand.md)）。

## 后端契约

后端只回答两件事："这个会话的文件树在哪"和"怎么启动一个能看到它的进程"。git、会话、镜像都在 `internal/manager` 和 `internal/repo`。接口见 `internal/backend/backend.go`。

| 后端 | 平台 | 视图路径 | 机制 |
|---|---|---|---|
| `copy` | 全平台 | 每会话不同 | 整份复制 base，作为基准对照和兜底 |
| `overlay` | Linux（默认） | 固定，如 `/workspace/<repo>` | 每会话一个常驻 mount namespace，里面挂 overlayfs（lower=base，upper/work 在会话目录），`exec` 用 `nsenter` 加入（[0006](decisions/0006-overlay-persistent-mount-namespace.md)） |
| `clone` | macOS（默认） | 每会话不同 | APFS `clonefile` 写时复制；非 APFS 卷退回整份复制 |

约定：`Create` 只读 base，所有写入落在会话目录下（`Workspace.Shares` 里的共享路径除外：overlay 在 holder 里 bind mount，copy 和 clone 换成符号链接）；`Command` 的工作目录是视图根；`Remove` 要能处理只建了一半的工作区。

## 已知限制

- macOS 没有 mount namespace，做不到同一路径；需要严格一致时的备选是用一个常驻的轻量 Linux 虚拟机，尚未实现。
- `overlay` 依赖 `nsenter`（util-linux）；非 root 需要 Linux 5.11+ 且允许非特权 user namespace。
- `clone` 后端已在真实的 Mac 上验证：克隆生效（spawn 0.09 秒、新会话约 2 MB），`go test -race ./...` 通过。注意 `du` 会把克隆和 base 共享的数据块重复计入，要看真实占用只能比较 `df` 前后的差值。
- pnpm 在 overlay 下无法跨挂载点硬链接，依赖会整份复制；解决办法是让底层本来就带着装好的依赖：环境准备在 tingly-fuse 之前完成，再用 `git spawn --from-dir` 把这个目录当底层（[0004](decisions/0004-environment-prep-out-of-scope.md)、[0007](decisions/0007-from-dir-snapshot-and-absorb-objects.md)）。`--from-dir` 要求 `.git` 是目录，不支持 worktree。

## 尚未实现、仅在调研中的方向

见 [research/research-multi-agent-git-workspace.md](research/research-multi-agent-git-workspace.md)：按目录的共享/隔离策略、fsmonitor 与变更日志、checkpoint 与 fork、跨会话冲突预警、FUSE 后端。是否做、何时做取决于基准数据和实际使用情况，状态见 [roadmap.md](roadmap.md)。
