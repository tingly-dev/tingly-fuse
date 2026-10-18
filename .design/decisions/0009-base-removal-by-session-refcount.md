# 0009 base 按会话引用回收，删除与 spawn 用锁互斥

- 状态：已采纳
- 日期：2026-10-18

## 背景

base 只增不减：每个新 commit、每次 `--from-dir` 都多一份，旧的没有办法删（[issue #6](https://github.com/tingly-dev/tingly-fuse/issues/6) R5）。[0008](0008-pin-base-objects-in-mirrors.md) 之后，base 还在镜像里留着固定 ref，即使手动删掉目录，它独占的对象也回收不了。

## 决定

1. **引用就是会话记录。** 一个 base 被引用，当且仅当有会话的 `session.json` 记着它。`base rm` 遇到被引用的 base 拒绝，并列出会话；`base prune` 删除所有未被引用的 base，`--dry-run` 只列出。不做后台自动回收。
2. **删除顺序：** 先把 base 目录 rename 进 `bases/.tmp-rm-*`（`Bases()` 不列 `.tmp-` 开头的目录，所以不会被找到），再删它在所有镜像里的 `refs/tingly/bases/<id>/`，最后删目录。镜像里不再被需要的对象交给 git 的 gc 回收。
3. **与 spawn 互斥：** 仓库目录下的 `.bases.lock`。spawn 从选定 base 到写完 `session.json` 持共享锁；`base rm`/`prune` 持排他锁，在锁内重新统计引用。固定用另一把锁 `.pin.lock`，两者不嵌套。
4. **看不清就不删。** 有会话的 `session.json` 存在但读不出来时，`base rm`/`prune` 直接报错并列出这些会话，因为它可能引用任何 base。没有 `session.json` 的目录（spawn 失败的残留）不算引用。删镜像 ref 失败时把目录 rename 回来；`prune` 开始时先清掉上次中断留下的 `.tmp-rm-*`。`repo check --fix` 也持共享锁，不会和删除交错。
5. `base rm` 接受唯一前缀，方便用 `list` 里的短 id。

## 理由

- 会话记录是唯一可靠的引用来源：overlay 会话在整个生命周期都依赖 lower（holder 退出后要重新挂载），按"有会话就保留"处理最简单，对 clone/copy 后端也安全。
- 没有锁时，prune 可能在 spawn 选中 base 之后、会话落盘之前删掉它。`TestPruneDoesNotRaceSpawn` 去掉锁时稳定失败（8 个并发 spawn 只成功 1–7 个），加锁后全部成功且每个会话的 base 都在。

## 被放弃的选项

- **按时间自动清理。** 判断不了使用者是否还想用某个快照，删错了要重新准备环境。
- **clone/copy 会话不算引用。** 它们的视图确实不再读 base，但区分后端会让规则变复杂，先统一处理（issue #6 开放问题）。
- **base 标签（指向"最新准备好的环境"）。** 和回收是两件事，单独再做；有了标签后，被标签指向的 base 也要算作被引用。

## 后果

- `--from-dir` 的旧快照要使用者自己 `base prune`；tingly-box 上删掉一个 commit base 和一个带 node_modules 的快照，gc 后仓库目录从约 443 MB 降到约 294 MB。
- spawn 在建 base 期间（首次检出或快照，可能要几秒）持共享锁，这段时间 prune 会等待。
