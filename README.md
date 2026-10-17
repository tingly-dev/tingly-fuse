# tingly-fuse

让多个 agent 共用一个 git 仓库：每个 agent 拿到一个私有的、写时复制的工作区视图，从某个 commit 开始，只堆叠自己的分支，最后推到远端。不需要为每个 agent 开 worktree，也不需要容器编排。

设计记录在 [.design/](.design/)：[架构](.design/architecture.md)、[路线图](.design/roadmap.md)、[决策记录](.design/decisions/)和[前期调研](.design/research/research-multi-agent-git-workspace.md)。改动设计时请同步更新那里。

## 用法

```sh
go build -o tingly-fuse ./cmd/tingly-fuse

tingly-fuse repo add tingly-box https://github.com/tingly-dev/tingly-box
id=$(tingly-fuse spawn tingly-box --from main)     # 打印会话 id
tingly-fuse exec "$id" -- claude                   # 在视图里启动 agent
tingly-fuse push "$id"                             # 推送分支 agent/<id>
tingly-fuse drop "$id"
tingly-fuse list
tingly-fuse repo list -v                           # 带会话数和 base 数
tingly-fuse repo remove tingly-box --force         # 删除镜像和所有 base；--force 先 drop 它的会话
```

已经装好依赖、构建过的目录可以直接当底层（环境准备由你在 tingly-fuse 之前完成）：

```sh
id=$(tingly-fuse spawn tingly-box --from-dir ~/src/tingly-box)   # 快照该目录，含 node_modules 等被忽略的文件
tingly-fuse spawn tingly-box --base dir-…                        # 复用同一份快照（id 见 repo list -v），不再复制
```

`--from-dir` 用 reflink 复制，之后源目录怎么改都不影响已有会话；目录里未推送的提交、已暂存的文件和子模块对象会并入共享镜像。要求目录的 `.git` 是目录（不支持 worktree）。pnpm 项目注意：在 overlay 视图里 pnpm 会因设备号不同改用项目内 store 而重装，会话里设 `npm_config_store_dir` 指向准备环境时用的 store 目录即可（tingly-box 上 `pnpm install` 1.2 秒、额外约 12 MB，否则约 690 MB）。细节见 [决策 0007](.design/decisions/0007-from-dir-snapshot-and-absorb-objects.md)。

视图内运行的每个进程都会拿到 `TINGLY_WORKSPACE`（视图路径）、`TINGLY_SESSION`、`TINGLY_REPO` 三个环境变量。脚本和配置应读 `TINGLY_WORKSPACE`，不要写死路径：macOS 的 `clone` 后端每个会话的视图路径都不同。

状态目录默认是 `~/.tingly-fuse`，可用 `TINGLY_FUSE_HOME` 覆盖：

```
repos/<name>/objects.git    共享对象库（bare 镜像），所有对象只存在这里
repos/<name>/bases/<sha>/   某个 commit 的只读基础 checkout，.git 用 alternates 指向 objects.git
sessions/<id>/session.json  会话元数据
sessions/<id>/...           后端私有状态
```

## 后端

后端只负责"会话的文件树在哪里、怎么启动一个能看到它的进程"，git 和会话管理都在 `internal/manager`。接口见 [internal/backend/backend.go](internal/backend/backend.go)：

| 后端 | 平台 | 视图路径 | 状态 |
|---|---|---|---|
| `copy` | 全平台 | `sessions/<id>/root`，每会话不同 | ✅ 基准实现：整份复制基础 checkout |
| `overlay` | Linux（默认） | 固定路径 `/workspace/<repo>`，所有会话相同 | ✅ mount namespace + overlayfs，upper/work 在 `sessions/<id>/` |
| `clone` | macOS（默认） | `sessions/<id>/root`，每会话不同 | ✅ APFS clonefile 写时复制；非 APFS 卷自动退回整份复制 |

`overlay` 后端说明：

- 每个会话只有一个 mount namespace 和一个 overlay 挂载（lower = 基础 checkout，upper/work 在会话目录），由常驻的 holder 进程持有；每次 `exec` 用 `nsenter`（util-linux）加入它，所以同一会话的并发 `exec` 看到的是同一个挂载。宿主和其他会话都看不到这个挂载，宿主上的 `/workspace/<repo>` 只是一个空的挂载点。
- holder 的 pid 和 namespace 记在 `sessions/<id>/holder.json`。holder 意外退出时，如果还有命令在原 namespace 里运行就加入它，否则重新挂载（写入都在 upper 里，不会丢）；`drop` 时结束 holder。
- 视图根目录默认 `/workspace`，可用 `TINGLY_FUSE_VIEW_ROOT` 覆盖（在 spawn 时生效，之后记录在会话元数据里）。
- root 下只用 `CLONE_NEWNS`；非 root 时额外用 user namespace，把自己的 uid/gid 映射为自身，需要 Linux 5.11+ 且允许 unprivileged user namespace。非 root 用户需要先能创建挂载点：`sudo mkdir -p /workspace && sudo chown $USER /workspace`，或改用 `TINGLY_FUSE_VIEW_ROOT`。
- 内核不支持时用 `--backend copy`。

实现新后端的约定：

- `Create` 只能读 `BaseDir`，绝不能写；所有写入落在 `StateDir` 之下。
- `Command` 返回的进程工作目录必须是视图根目录；不要覆盖 `cmd.Env` 里已有的变量，manager 会在其后追加。
- `Remove` 必须能处理只创建了一半的工作区。
- 在 `init` 里调用 `backend.Register`，并在 `cmd/tingly-fuse/main.go` 里匿名导入；平台默认值通过 `backend.Default` 设置。

## 测试

```sh
go test -race ./...
```
