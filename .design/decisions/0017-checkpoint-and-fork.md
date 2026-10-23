# 0017 checkpoint 与 fork：把会话的树整份复制成新 base

- 状态：已采纳
- 日期：2026-10-23

## 背景

一个 agent 做到一半，想让另一个 agent 从它此刻的状态接着做，或者同时试两条路，现在只能让第二个会话从某个 commit 重来，会丢掉第一个会话里未提交的改动、装好的依赖和构建产物。[0007](0007-from-dir-snapshot-and-absorb-objects.md) 已经能把一个准备好的目录快照成 base，`git worktree add --base` 能从 base 起多个会话。overlay 视图在宿主上可见之后（[0015](0015-host-visible-overlay-view.md)），会话的树本身就是一个宿主能读的目录，可以直接当作那个目录。

## 决定

- **`checkpoint <session>`**：把会话视图的整棵树快照成该仓库的一个新 base，打印 base id。git 仓库走 `--from-dir` 的同一套逻辑（reflink 复制，对象并入镜像，origin 改回 upstream，加固定 ref）；dir 仓库走普通快照。未提交、已暂存、未跟踪的文件都在里面。
- **`fork <session> [--id] [-b branch] [--backend] [--share]`**：checkpoint，再用这个 base 起一个新会话（git 仓库上是 `git worktree add --base`，新分支默认 `agent/<id>`，在复制来的 HEAD 上建；dir 仓库是 `spawn --base`）。后端默认和父会话一样。标签不继承，需要的话 fork 之后用 `label`。
- **这一步只在一个 bases 锁（共享）下完成**：fork 从快照到新会话保存之间，`base prune` 取不走这个 base。创建新会话失败时，刚做的检查点会被删掉，不留垃圾。
- **共享路径不进检查点。** 视图里的共享路径显示的是共享的那一份（bind mount 或符号链接），复制后从 base 里删掉；从检查点起的新会话会自己重新共享它们。
- **版本不支持的情况明确报错：** `--shared-git` 的会话（它的 `.git` 是指向共享仓库的符号链接，工作树状态不在视图里）；只在会话 namespace 里可见的视图（`overlay-ns` 和旧版 overlay 会话），宿主复制不到。
- 检查点就是普通 base：被会话用着就不会被 `base prune` 回收，没有会话用就会。想长期留着一个只做备份的检查点，需要先 fork 一个会话占着它，或者别跑 prune；这一版不加“保留”标记。
- 顺带修了一个 id 冲突：`--from-dir` 快照的 id 是 `dir-<head前12位>-<秒级时间>`，同一秒对同一个 commit 做两次快照（连续 fork 两次）会重名，rename 失败。现在加了 6 位随机后缀。

## 理由

- 复用 `--from-dir` 的快照：对象并入镜像、固定 ref、origin 处理都已经验证过（0007、[0008](0008-pin-base-objects-in-mirrors.md)），不需要新的 base 模型。
- 整份复制在支持 reflink 的文件系统上几乎免费（XFS、btrfs、APFS），在 ext4 上是真复制（tingly-box 约 724 MB）。第一版接受这个代价，换来所有后端都能用、base 仍是一个独立的只读目录。

## 被放弃的选项

- **分层 base：只复制 upper，新 base 记父 base，overlay 用多层 lowerdir。** 最省盘，但 base 变成层链，`prune` 要看父子，copy 和 clone 后端要在创建时按层展开，改动面大得多。等有真实用量、确认整份复制太贵再做。
- **checkpoint 时冻结会话（暂停进程）。** tingly-fuse 不管进程，做不到；文档要求调用方先停写。

## 后果

- 会话还有进程在写时，检查点可能不一致（文件写到一半）。
- ext4 上每个检查点占一整份，大仓库不适合频繁 fork；XFS、btrfs、APFS 上很便宜。
- 并入镜像的对象不会自动清理（同 0007）。
