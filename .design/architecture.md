# 架构

## 目标

让多个 agent 同时使用同一个 git 仓库，不为每个 agent 开 worktree，也不做容器编排。每个 agent 得到一个私有的、写时复制的工作区视图，从某个 commit 出发，只在自己的分支上堆叠改动，推到远端后再处理合并。

## 核心原则

1. **共享存储，隔离工作空间。** 对象库和未改动的文件所有会话共享；工作区文件、index、HEAD 每会话一份。（[0003](decisions/0003-shared-objects-via-alternates.md)）
2. **路径一致。** 在支持的平台上，所有会话在同一个路径下工作，背后是各自的视图。Go 构建缓存按绝对路径命中，路径一致能让第二个会话起的构建从约 150 秒降到约 15 秒。（[0002](decisions/0002-same-path-private-view.md)）
3. **合并交给远端。** 会话只负责产出分支并推送，本地不做合并。（[0005](decisions/0005-merge-on-remote.md)）
4. **职责边界。** 环境准备（装依赖、构建）发生在 tingly-fuse 之前，不归它管。它只解决"拿到一个现成的底层之后，多个 agent 如何互不干扰"。（[0004](decisions/0004-environment-prep-out-of-scope.md)）

## 状态目录

默认 `~/.tingly-fuse`，可用 `TINGLY_FUSE_HOME` 覆盖。

```
repos/<name>/repo.json          登记信息（upstream）
repos/<name>/objects.git        bare 镜像，所有 git 对象只存在这里
repos/<name>/modules/<h>.git    子模块的 bare 镜像，按 URL 哈希，跨 base 共用
repos/<name>/bases/<sha>/       某个 commit 的只读基础 checkout
sessions/<id>/session.json      会话元数据（repo、base、分支、后端、视图路径）
sessions/<id>/...               后端私有状态（overlay 的 upper/work、holder.json 等）
```

基础 checkout 的 `.git` 以及它的子模块仓库都只带 alternates，指向上面的镜像，自己不存对象。原因见 [0003](decisions/0003-shared-objects-via-alternates.md)。

## 命令与流程

| 命令 | 做什么 |
|---|---|
| `repo add/list/remove` | 登记仓库并建镜像；列出（`-v` 带会话数和 base 数）；删除（`--force` 先 drop 它的会话） |
| `spawn <repo> [--from rev]` | 解析 rev 得到 commit；确保该 commit 的 base 存在（并发安全，先建临时目录再 rename）；预留会话 id；后端 `Create` 出视图；在视图里建分支 `agent/<id>` |
| `exec <session> -- cmd...` | 在会话视图里运行命令，注入 `TINGLY_WORKSPACE`、`TINGLY_SESSION`、`TINGLY_REPO` |
| `push <session>` | 在视图里把分支推到真实的 upstream（base 的 origin 指向 upstream，不是本地镜像） |
| `drop <session>` | 后端 `Remove` 并删除会话目录，未推送的工作随之丢失 |
| `list` | 列出会话 |

`spawn` 失败时会回收已创建的一切（视图和会话目录），不留半成品。

## 后端契约

后端只回答两件事："这个会话的文件树在哪"和"怎么启动一个能看到它的进程"。git、会话、镜像都在 `internal/manager` 和 `internal/repo`。接口见 `internal/backend/backend.go`。

| 后端 | 平台 | 视图路径 | 机制 |
|---|---|---|---|
| `copy` | 全平台 | 每会话不同 | 整份复制 base，作为基准对照和兜底 |
| `overlay` | Linux（默认） | 固定，如 `/workspace/<repo>` | 每会话一个常驻 mount namespace，里面挂 overlayfs（lower=base，upper/work 在会话目录），`exec` 用 `nsenter` 加入（[0006](decisions/0006-overlay-persistent-mount-namespace.md)） |
| `clone` | macOS（默认） | 每会话不同 | APFS `clonefile` 写时复制；非 APFS 卷退回整份复制 |

约定：`Create` 只读 base，所有写入落在会话目录下；`Command` 的工作目录是视图根；`Remove` 要能处理只建了一半的工作区。

## 已知限制

- macOS 没有 mount namespace，做不到同一路径；需要严格一致时的备选是用一个常驻的轻量 Linux 虚拟机，尚未实现。
- `overlay` 依赖 `nsenter`（util-linux）；非 root 需要 Linux 5.11+ 且允许非特权 user namespace。
- `clone` 后端已在真实的 Mac 上验证过克隆生效（spawn 0.09 秒、新会话约 2 MB），但 `TestCloneTreeNative` 的结果还没拿到。注意 `du` 会把克隆和 base 共享的数据块重复计入，要看真实占用只能比较 `df` 前后的差值。
- pnpm 在 overlay 下无法跨挂载点硬链接，依赖会整份复制；解决办法是让底层本来就带着装好的依赖，这属于环境准备，见 [0004](decisions/0004-environment-prep-out-of-scope.md) 和 roadmap 中的 issue #1。

## 尚未实现、仅在调研中的方向

见 [research/research-multi-agent-git-workspace.md](research/research-multi-agent-git-workspace.md)：按目录的共享/隔离策略、fsmonitor 与变更日志、checkpoint 与 fork、跨会话冲突预警、FUSE 后端。是否做、何时做取决于基准数据和实际使用情况，状态见 [roadmap.md](roadmap.md)。
