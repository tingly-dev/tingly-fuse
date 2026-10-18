# 0008 在镜像里固定 base 需要的对象，分支名先 fetch 再解析

- 状态：已采纳
- 日期：2026-10-18

## 背景

[0003](0003-shared-objects-via-alternates.md) 让 base 和视图都不存对象，只通过 alternates 借用镜像。但 git 在镜像里 gc 时只把镜像自己的 ref 当作根，看不到借用者。于是：

- 上游删分支或 force-push 后，镜像 `fetch --prune` 删掉对应 ref，base 的 commit 变成不可达；
- `--from-dir` 并入的未推送提交、暂存的 blob、stash、reflog 里的 commit 从一开始就不可达（[0007](0007-from-dir-snapshot-and-absorb-objects.md)）；
- 子模块镜像是 `--mirror` 克隆，refspec 是 `+refs/*:refs/*`，`fetch --prune` 会删掉任何上游没有的 ref；按 id 取来的 commit 本来就没有 ref。

fetch 会触发 `gc --auto`，不可达对象过了保留期（默认两周）就会被删。`internal/manager/lifecycle_test.go` 用 `gc --prune=now` 模拟这一刻，修复前所有场景的视图里 `git status` 都报 `fatal: bad object HEAD`。

另外，`Resolve` 只在 rev 本地解析不到时才 fetch，`spawn --from main` 会悄悄用镜像里旧的 main。

## 决定

1. **每个 base 在它借用的每个镜像里固定自己需要的对象**，ref 放在 `refs/tingly/bases/<id>/<label>/` 下，`label` 是 `root` 或 `m-<子模块 gitdir 相对路径的哈希>`：
   - `keep`：一个合成的 commit，tree 是 index 里所有条目（冲突的条目按 stage 加后缀保留），parent 是 HEAD、gitdir 里每个指向 commit 的 ref（分支、远端跟踪分支、轻量 tag、stash）以及 reflog 里出现过的所有 commit；
   - `r/<原 ref 名>`：指向非 commit 对象的 ref（附注 tag），commit 不能把它们当 parent，只好逐个复制。
2. **root 的 `keep` 最后写，作为"已固定"的标记。** `PinBase` 有标记就直接返回，否则在仓库级 flock 下固定。`spawn` 每次都调用它，所以修复前建的 base 在下次被用时自动补上。
3. **子模块镜像的 refspec 改为只管 `refs/heads/*` 和 `refs/tags/*`**，`fetch --prune` 不再碰 `refs/tingly/`。创建、fetch 前、固定时都会设置，旧镜像也会被改过来。
4. **`--from` 除完整 commit id 外都先 fetch 再解析**；fetch 失败直接报错并提示 `--no-fetch`，不悄悄退回旧值。
5. **只固定镜像里完整存在的对象。** 写 keep 前用 `rev-list --objects --missing=print` 检查每个 parent，缺对象的 commit 不放进 keep，index 里缺失的 blob 也不进 tree。否则固定 ref 指向缺失对象，镜像以后的 gc 会直接失败（`unable to read`），整个镜像再也回收不了。缺的东西由 `Fsck` 报出来。
6. **每个镜像一把锁 `tingly-fuse.lock`**，fetch、按 id fetch、改 refspec、写固定 ref 都在锁内。并发 spawn 同时 fetch 会在同一个 ref 上抢锁失败（8 个并发只成功 1 个）；有锁后，等锁的 spawn 发现锁内已有一次在它开始之后完成的 fetch，就不再 fetch。
7. **`repo check [--fix]` 是现场的验证手段**：逐个 base 报告是否已固定、`git fsck --connectivity-only` 是否通过；`--fix` 只补固定，已经丢掉的对象找不回来，只会被报出来。

## 理由

- 固定 ref 是 git 自己认的根，不用改 gc 配置，也不用 `gc.auto=0` 关掉回收（那样镜像会无限增长）。
- 固定 base 的全部 ref 而不只是 HEAD：视图里的 `origin/*`、本地分支、stash 指向缺失对象时，`git log`、`fetch` 协商、`fsck` 都会出错。
- 指向 commit 的 ref、reflog 和 index 用一个合成 commit 收拢：tingly-box 的一个 base 每个 gitdir 带约 900 个远端跟踪分支和 tag，逐个复制时 3 个 base 就在镜像里留下约 5500 个 ref，收拢后每个 gitdir 只剩 1 个 keep 加几个附注 tag。index 里的 blob 不在任何 commit 里，只能靠 tree 固定。
- 每个 base 一组前缀，将来 `base prune` 删 base 时一并删掉这组 ref，镜像的 gc 就能回收它独占的对象（[issue #6](https://github.com/tingly-dev/tingly-fuse/issues/6)）。

## 被放弃的选项

- **关掉镜像的 gc。** 能避免误删，但镜像只增不减，删了 base 也回收不了空间。
- **每个 base 自己留一份对象（不借用镜像）。** 回到 0003 之前，每个 base 多出整个 pack，overlay 下还会触发 pack freshen 的复制。
- **只固定 HEAD。** 视图里其余 ref 和 index 仍会损坏，`fsck` 不过。
- **`--from` 继续只在解析不到时 fetch。** 快，但结果取决于镜像上次何时更新，而且没有任何提示。

## 后果

- 每个 base 首次使用时多一次固定：tingly-box（4 个 gitdir）约 0.5 秒；之后每次 spawn 只多一次 `rev-parse`。
- 镜像里每个 base 每个 gitdir 多 1 个 keep ref 和若干附注 tag ref；`clone --shared` 和 fetch 只取 `refs/heads/*`（和 tag），不受影响。
- 默认 `spawn` 需要能访问 upstream；离线时用 `--no-fetch` 或完整 commit id。
- 固定只保护 base 里的东西。会话自己新建的对象写在会话目录（overlay 的 upper、copy/clone 的视图）里，不依赖镜像。
