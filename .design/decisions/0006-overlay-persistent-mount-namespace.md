# 0006 overlay 后端每会话一个常驻 mount namespace

- 状态：已采纳；新会话的 `overlay` 改为在宿主上挂载（[0015](0015-host-visible-overlay-view.md)），本条描述的做法现在是 `overlay-ns` 后端，已有会话照旧
- 日期：2026-10-17

## 背景

第一版 overlay 后端在每次 `exec` 时新建 mount namespace 并挂载该会话的 overlay。同一会话并发 `exec`（例如 agent 在运行，同时外部执行 `push`）会用同一组 upper/work 挂载两次，内核报：

> overlayfs: upperdir is in-use as upperdir/workdir of another mount, accessing files from both mounts will result in undefined behavior.

## 决定

每个会话只有一个 mount namespace 和一个 overlay 挂载，由一个常驻的 holder 进程持有（re-exec 自身，挂好后通过 fd 报告再等 SIGTERM）。每次 `exec` 用 `nsenter` 加入这个 namespace，并通过一个 chdir 小跳板进入视图目录（`nsenter --wd` 在进入 namespace 之前解析路径，会落到宿主上的空目录）。

holder 的 pid 和 mnt namespace 的 inode 记在 `sessions/<id>/holder.json`（inode 用来防 pid 复用），用 flock 串行化。holder 意外退出时：如果原 namespace 里还有进程就加入它，否则重新挂载；写入都在 upper 里，不会丢。`drop` 时结束 holder。

## 理由

- 同一会话的并发命令看到同一个挂载，写入互相可见，内核不再告警。
- 不依赖 Go 里无法实现的 `setns` 进入 mount namespace（Go 多线程进程不能直接做）。

## 后果

- 新增运行时依赖 `nsenter`（util-linux）。
- 两个测试覆盖：`TestConcurrentExecSharesOneMount`（并发 `exec` 互相可见、同一个 namespace、视图处只有一个挂载）和 `TestHolderRecovery`（holder 被杀后重建且数据还在；有存活命令时加入其 namespace；`drop` 后 holder 退出）。
