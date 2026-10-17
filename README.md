# tingly-fuse

让多个 agent 共用一个 git 仓库：每个 agent 拿到一个私有的、写时复制的工作区视图，从某个 commit 开始，只堆叠自己的分支，最后推到远端。不需要为每个 agent 开 worktree，也不需要容器编排。

设计背景见 [docs/research-multi-agent-git-workspace.md](docs/research-multi-agent-git-workspace.md)。

## 用法

```sh
go build -o tingly-fuse ./cmd/tingly-fuse

tingly-fuse repo add tingly-box https://github.com/tingly-dev/tingly-box
id=$(tingly-fuse spawn tingly-box --from main)     # 打印会话 id
tingly-fuse exec "$id" -- claude                   # 在视图里启动 agent
tingly-fuse push "$id"                             # 推送分支 agent/<id>
tingly-fuse drop "$id"
tingly-fuse list
```

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
| `overlay` | Linux | 固定路径（如 `/workspace/<repo>`），所有会话相同 | 待实现：mount namespace + overlayfs |
| `clone` | macOS（默认） | `sessions/<id>/root`，每会话不同 | ✅ APFS clonefile 写时复制；非 APFS 卷自动退回整份复制 |

实现新后端的约定：

- `Create` 只能读 `BaseDir`，绝不能写；所有写入落在 `StateDir` 之下。
- `Command` 返回的进程工作目录必须是视图根目录；不要覆盖 `cmd.Env` 里已有的变量，manager 会在其后追加。
- `Remove` 必须能处理只创建了一半的工作区。
- 在 `init` 里调用 `backend.Register`，并在 `cmd/tingly-fuse/main.go` 里匿名导入；平台默认值通过 `backend.Default` 设置。

## 测试

```sh
go test -race ./...
```
