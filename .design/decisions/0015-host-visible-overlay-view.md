# 0015 overlay 视图在宿主上可见

- 状态：已采纳，部分取代 [0006](0006-overlay-persistent-mount-namespace.md)（对新会话；已有会话照旧）
- 日期：2026-10-22
- Issue：[#12](https://github.com/tingly-dev/tingly-fuse/issues/12)

## 背景

[0014](0014-per-session-view-path.md) 之后每个会话有自己的视图路径，但 overlay 只挂在会话的 mount namespace 里（[0006](0006-overlay-persistent-mount-namespace.md)），宿主上 `sessions/<id>/root` 是空目录。人和 IDE 打不开，只能 `exec` 进去看；copy 和 clone 没有这个问题。内核 overlayfs 非 root 只能挂在 user namespace 里，所以宿主可见必须分 root 和非 root 两条路。

## 决定

Linux 上有三个 overlay 后端：

| 后端 | 谁能用 | 宿主可见 | 机制 |
|---|---|---|---|
| `overlay` | root | 是 | 在宿主 mount namespace 里直接挂内核 overlayfs 到 `sessions/<id>/root` |
| `fuse-overlay` | 任何用户 | 是 | `fuse-overlayfs` 挂在同一位置，走 FUSE |
| `overlay-ns` | root 和非 root | 否 | 原来的做法：常驻 holder + `nsenter`（0006），作为显式选项保留 |

**默认值：root 用 `overlay`，非 root 用 `fuse-overlay`。** 后端不可用时（非 root 用 `overlay`；没有 `fuse-overlayfs` 或 `/dev/fuse`）`spawn` 直接报错并指出可选的后端，**不自动退回 copy**（沿用"让它崩"的约定）。

### 宿主挂载的细节

- **没有 holder，没有 `nsenter`。** 挂载在 `spawn` 时做，之后进程退出挂载仍在。`exec` 直接以视图为工作目录运行命令。
- **每次 `exec` 先检查挂载**，没有就重新挂（机器重启、管理员 `umount` 之后）。upper 在会话目录里，写入不丢。检查和挂载在会话锁（flock）下，同一个 upper 不会挂两次。
- **FUSE 守护进程死了**，挂载还在但任何访问都报 `ENOTCONN`；下一次 `exec` 先卸掉再重挂。
- **共享路径（[0011](0011-shared-paths.md)）**：`overlay` 在宿主上 bind mount，和 `overlay-ns` 一样，被跟踪的路径也能共享。`fuse-overlay` 非 root 不能 bind mount，改用符号链接，和 copy、clone 一样，被跟踪的路径会被拒绝。
- **`drop` 先卸载。** 卸载会话视图下的所有挂载（共享路径的 bind mount 先于 overlay），不用懒卸载：有进程还在视图里时 `drop` 报错并保留会话，之后可以重试。这一点很重要：共享路径挂在视图里，如果卸载失败还去删会话目录，`RemoveAll` 会顺着 bind mount 删到共享目录。所以 `Drop` 和 spawn 失败的清理，在后端 `Remove` 失败时都不再删会话目录。
- **已有会话**：session.json 里记着 `overlay`、会话目录里有 `holder.json` 的，继续按 `overlay-ns` 的方式工作（`Create/Command/Remove` 都按 `holder.json` 分流），升级不需要迁移。

### 非 root 的取舍

在 tingly-box（含子模块，依赖已装进 base）上，各量了 3 个会话，数据在 [../../bench/hostview.md](../../bench/hostview.md)：

| 每个会话 | 创建 | 占盘 | 宿主上 `git status` | vite build | go build（预热缓存） |
|---|---|---|---|---|---|
| `overlay`（root，基准） | 0.02 s | 0.1 MB | 0.05 s | 3.7 s | 10.9 s |
| `fuse-overlay`（ext4） | 0.02 s | 0.1 MB | 0.28 s | 4.6 s | 11.3 s |
| `copy`（ext4，真复制） | 3.1 s | 724 MB | 0.03 s | 3.5 s | 12.4 s |
| `copy`（XFS，`cp -a` 自动 reflink） | 6.1 s | 64 MB | 0.04 s | 3.7 s | 13.2 s |

- `fuse-overlay` 创建几乎免费、不占盘，构建比内核 overlay 慢 0%–25%，宿主上扫一遍工作区（IDE 打开目录就是这种读）约 0.3 s。
- `copy` 在 ext4 上要 724 MB、3 秒；在 XFS 上靠 reflink 只占 64 MB，但创建要 6 秒（逐文件 reflink，比 ext4 上的复制还慢），而且要求文件系统支持。
- 所以非 root 默认 `fuse-overlay`。没有 FUSE 的环境（容器里没有 `/dev/fuse`）由用户显式选 `--backend copy`。

## 理由

- 内核 overlayfs 直接挂在宿主上最简单：没有 holder、没有 `nsenter`、不用 user namespace，也不依赖 Linux 5.11+；并发 `exec` 看到的是同一个挂载，不需要像 0006 那样专门处理。
- 非 root 想宿主可见，只有 FUSE 和复制两条路，数据显示 FUSE 在速度和占盘上都明显好于复制。

## 被放弃的选项

- **非 root 默认退回 copy / reflink。** 不是所有文件系统都有 reflink，ext4 上 724 MB、3 秒，XFS 上也更慢；
- **只保留 `overlay-ns`。** 就是现状，人和 IDE 打不开。把它留作显式选项，是因为它是不需要 FUSE 的非 root 办法。
- **自动在 `overlay` 不可用时换成 `fuse-overlay` 或 copy。** 同一个名字在不同用户下行为不同，出问题时不知道自己用的是什么。默认值按用户选，名字对应的行为固定。

## 后果

- 新增运行时依赖：非 root 要 `fuse-overlayfs` 和可用的 `/dev/fuse`；root 用 `overlay` 不再需要 `nsenter`（`overlay-ns` 仍要）。
- root 下 `spawn` 之后，宿主的 `findmnt` 会列出每个会话的挂载（overlay，加每个共享路径的 bind mount）。机器重启后挂载消失，下一次 `exec` 才重挂；直接在宿主上打开一个没人 `exec` 过的会话目录，重启后看到的是空目录，需要先运行 `tingly-fuse mount [会话...]`（不带会话就挂全部；可放进开机启动）或 `exec` 一次。`mount` 对已挂载的视图什么都不做，对 copy、clone 也什么都不做。
- 宿主上的进程能写会话的视图，也就能写 upper；这是目的，但意味着人和 agent 可以同时改同一个文件，tingly-fuse 不做协调。
- `fuse-overlay` 的 pnpm 行为：ext4 上 `pnpm install`（依赖已在 base 里）约 11 秒，XFS 上约 2 秒，kernel overlay 上约 1.7 秒。原因还没查清，不影响选择。
