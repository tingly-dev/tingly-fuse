# 0002 同一路径，每会话私有视图

- 状态：已采纳
- 日期：2026-10-17

## 背景

多个 agent 共用一个仓库时，冲突出在共用同一个工作区，而不在共用仓库。worktree 每个 agent 一个目录，路径各不相同。

## 决定

在 Linux 上，所有会话在同一个路径（默认 `/workspace/<repo>`）下工作，每个会话进入自己的 mount namespace，把自己的 overlayfs 挂在这个路径上。macOS 没有 mount namespace，用 APFS clonefile，每会话一个路径。

共享还是隔离，本质是 IO 层的路由策略。目前只实现了"完全隔离"。

## 理由

- 路径是个名字，内核解析时查的是进程所在的 mount namespace 的挂载表，所以同一个字符串可以指向不同内容，agent 完全无感。
- 路径一致让 Go 构建缓存（按绝对路径命中）跨会话复用：基准测试里第二个会话起 `go build` 约 15 秒，worktree、copy、reflink 都要约 150 秒。（[bench/README.md](../../bench/README.md)）
- agent 的提示词、脚本、配置可以统一写同一个路径。

## 被放弃的选项

- 单个 FUSE 挂载点按调用者 pid 区分内容：内核页缓存和目录项缓存被所有调用者共享，必须关缓存，性能差，子进程归属也容易判错。
- 每会话一个不同路径（worktree 式）：丢掉构建缓存复用。macOS 上只能接受这一点。

## 后果

- macOS 默认拿不到同一路径。需要严格一致时的备选是一个常驻轻量 Linux 虚拟机，尚未实现。
- 脚本和工具应读 `TINGLY_WORKSPACE`，不要写死路径，这样两类后端都能用。
- Go 构建可加 `-trimpath` 缓解路径不同的影响（路径不进缓存键）。
