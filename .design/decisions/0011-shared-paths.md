# 0011 任意目录或文件可在会话之间共享

- 状态：已采纳
- 日期：2026-10-20

## 背景

[0010](0010-shared-git-via-registered-worktrees.md) 只解决了 `.git`。YZ 的要求是通用的：任何目录或文件都可以设为共享，`.git` 是其中需要特殊处理的一个，用选项支持。调研里"按目录混合"的策略（[research](../research/research-multi-agent-git-workspace.md) 4.0 节）也是这个意思：源码隔离，缓存、产物、配置这类目录共享。

## 决定

1. **按仓库配置共享路径。** `repo share <repo> <path>...` 添加，`repo unshare <repo> <path>...` 移除，`repo share <repo>` 列出；也可以在 `repo add --share <path>`（可重复、可逗号分隔）时一并设置。存在 `repo.json` 的 `shared_paths` 里。路径相对于仓库根，可以是目录也可以是文件。`spawn --share <path>` 给单个会话再加共享路径：它和其他也带这个路径 spawn 的会话共用同一份副本，没带的会话照常私有。
2. **每个共享路径在宿主上只有一份：`repos/<name>/shares/<path>`。** 第一次有会话带着它 spawn 时创建：base 里有这个路径就复制一份（reflink），没有就建一个空目录。之后 base 里的这份不再使用。要共享一个文件，它得出现在 base 里（例如 `--from-dir` 目录里的 `.env`），或者先放到 `shares/` 下。
3. **怎么出现在视图里由后端决定。** `backend.Workspace` 新增 `Shares`。
   - overlay：holder 挂好 overlay 后，把每个共享路径 bind mount 到视图里的同一位置。路径不变（[0002](0002-same-path-private-view.md)），而且对挂载点的 rename 会报 `EBUSY`，工具"写临时文件再改名"时直接报错，不会悄悄把共享变回私有。holder 重启后重新挂。
   - copy、clone：把视图里的路径换成指向共享副本的符号链接。
4. **会话的共享路径在 spawn 时确定**，记在 `session.json` 的 `shared` 里。之后改配置只影响新会话。
5. **明确报错，不做兜底：**
   - `.git` 和 `.git/` 下的路径拒绝，提示用 `repo add --shared-git`：会话必须有自己的 HEAD 和 index，`.git` 的共享方式见 [0010](0010-shared-git-via-registered-worktrees.md)。
   - 绝对路径、`..`、互相嵌套的两个路径拒绝。
   - copy、clone 后端上，git 跟踪着的路径拒绝：git 会把符号链接看成被跟踪的内容被替换了，agent 一提交就会把它从上游删掉。overlay 上允许，改动会出现在每个会话的 `git status` 里。
   - 视图里路径上有符号链接、或者 base 和共享副本一个是文件一个是目录，spawn 失败。

## 理由

- 共享的单位是路径，不是"某种已知目录"。`.git` 例外，因为它里面一半要共享（refs、对象），一半必须私有（HEAD、index）。
- 宿主上一份副本，所有后端都能用；bind mount 只在 overlay 上有（它本来就在独立的 mount namespace 里），其他后端用符号链接，不需要额外权限。

## 被放弃的选项

- **所有后端都用符号链接。** overlay 上能用 bind mount 保持路径一致、挡住 rename，没有理由降级。
- **共享副本一直跟着 base 变。** 不同 base 的内容不同，跟着变就没有"同一份"可言。

## 后果

- 共享路径里的写入没有隔离，并发写不安全的工具（一些构建缓存）会互相破坏，需要使用者判断哪些目录适合共享。
- `unshare` 不删除 `shares/` 下的内容，`repo remove` 时一并删除。
- copy、clone 上的符号链接可以被工具删掉或用 rename 替换，那样这个会话就悄悄不再共享了；overlay 没有这个问题。

## 验证（2026-10-20，Linux，copy 和 overlay，root 与 uid 65534）

`internal/manager/shares_test.go`：两个会话共享 `cache` 和 `build/out`，写、删、新建互相立即可见，其余文件保持私有；drop 一个会话不影响另一个；overlay 上杀掉 holder 后重建，共享路径仍在（把 bind mount 去掉时这个测试失败）；`unshare` 只影响新会话；共享被跟踪的 `README.md`，overlay 上改动出现在另一个会话的 `git status` 里、rename 覆盖它会失败，copy 上 spawn 报错且不留会话；`--from-dir` 里未跟踪的 `.env` 作为文件共享，后来从 commit 出发的会话也拿到同一份；`.git`、`../x`、绝对路径、嵌套路径都被拒绝。
