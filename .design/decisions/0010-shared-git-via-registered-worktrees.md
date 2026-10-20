# 0010 共享 .git：每个会话是共享仓库的一个已登记、已锁定的 worktree

- 状态：已采纳
- 日期：2026-10-20

## 背景

[0003](0003-shared-objects-via-alternates.md) 之后，会话之间只共享上游已有的对象。每个会话的 `.git`（refs、config、自己写出的对象）仍然私有：一个会话的提交，别的会话要等它推到远端再 fetch 才看得到；每个会话各自 fetch，各自存一份新对象。YZ 提出能否让一些目录、尤其是 `.git`，在所有会话之间共享。

## 决定

`repo add <name> <upstream> --shared-git` 按仓库开启，登记时确定，之后不改。开启后：

1. **一个共享仓库 `repos/<name>/shared.git`。** bare，alternates 指向镜像，origin 是真实 upstream，fetch refspec 是 `refs/heads/*:refs/remotes/origin/*`。refs、config 和会话新写的对象都在这里，所有会话看到的是同一份。
2. **每个会话是它的一个 worktree，在 `shared.git/worktrees/<id>/` 登记**：HEAD（从 base 的 HEAD 分离出发）、index、子模块的 gitdir（`modules/`，从 base 复制）、`commondir`、`gitdir`、`locked`。视图里的 `.git` 换成指向这个目录的符号链接。这一步由视图里的进程完成（overlay 下只有 namespace 里能改视图）。
3. **每个 worktree 都加锁**（`locked` 文件）。overlay 下所有会话的路径都是 `/workspace/<repo>`，在宿主上不存在，不加锁的话任何一次 `git worktree prune`（包括 gc 自动做的那次）都会把别的会话的登记删掉。
4. **子模块配置放在每个会话的 `config.worktree`**（开启 `extensions.worktreeConfig`），因为不同 base 的子模块可能不同。`core.bare` 移进 shared.git 自己的 `config.worktree`，否则会对所有会话生效。子模块 gitdir 里的相对 `core.worktree` 改成视图里的绝对路径：原值相对于 base 里 gitdir 的位置，从 shared.git 里解析就落空了。
5. **镜像替 shared.git 的 refs 保留对象。** 会话 fetch 时，镜像已有的对象不会再存进 shared.git；上游删了分支、镜像 gc 后，shared.git 里的 `refs/remotes/origin/<分支>` 就指向缺失的对象，之后每个会话的 gc 都失败。所以每次 fetch 镜像之前，先把 shared.git 的 refs 同步到镜像的 `refs/tingly/shared/` 下（带上只有 shared.git 有的少量对象）。
6. **`drop` 只删这个会话的登记。** 它的分支和提交留在 shared.git 里，别的会话还能用；不要了就在任一会话里 `git branch -D`。

## 理由

- 这就是 git 自己的 worktree 存储方式，git 的各种检查都认它：一个会话 `gc --prune=now` 不会删掉另一个会话暂存的 blob；两个会话不能检出同一个分支，git 会直接报错。
- 实测（git 2.43）只写 `commondir`、不在 `worktrees/` 下登记时，另一个会话的 `gc --prune=now` 删掉了这个会话暂存的 blob。登记是必须的。
- 不改 base 的形态：base 还是 [0003](0003-shared-objects-via-alternates.md) 的样子，固定 ref（[0008](0008-pin-base-objects-in-mirrors.md)）和回收（[0009](0009-base-removal-by-session-refcount.md)）的逻辑不用动，私有模式和共享模式用同一套 base。

## 被放弃的选项

- **直接用镜像当共享仓库。** 镜像的 `fetch --prune` 用 `refs/heads/*:refs/heads/*`，会把上游没有的 `agent/*` 分支删掉。
- **只写 `commondir`、不登记。** 见上面的实测：gc 看不到这个会话的 index 和 HEAD。
- **`gc.pruneExpire=never` 代替登记。** 挡不住手动的 `gc --prune=now`，也挡不住两个会话检出同一个分支。
- **视图里 `.git` 写成 `gitdir:` 文件。** 子模块工作区里的 `.git` 文件写的是 `../../.git/modules/...`，要求 `.git` 是个目录；符号链接保持这条路径有效。
- **默认开启。** 共享以后 refs 和 config 不再隔离，是语义变化，要使用者自己选。

## 后果

- 一个会话的 `git branch -D`、`git config`、hook、`git gc` 会影响所有会话。并发更新同一组 ref 时 git 可能因为锁文件报错，这些错误原样返回，不重试。
- 只共享主仓库；子模块的 gitdir 仍然每会话一份（和私有模式一样，对象走子模块镜像）。
- `--from-dir` 的 base 在共享模式下只带过来 HEAD（分离）、index 和子模块；它的本地分支、stash、hook、`info/exclude` 和子模块以外的 config 不带过来，refs 和 config 以 shared.git 为准。
- 删掉的会话留下的分支和对象不会自动清理；`repo remove` 时连同 shared.git 一起删除。
- 已登记的仓库不能中途切换模式，要换就重新 `repo add`。

## 验证（2026-10-20，Linux，copy 和 overlay，root 与 uid 65534）

`internal/manager/sharedgit_test.go`，每个后端都跑：会话 a 的提交在 b 里立刻可见，b 不能检出 a 的分支；a 在子模块里提交不影响 b 的 `git status`；a 里 `reflog expire` 加 `gc --prune=now` 加 `worktree prune` 之后，b 暂存的 blob 还在，两个会话 `fsck` 通过；`drop a` 后 a 的分支仍在、b 的登记没被 prune 掉。`TestSharedRefsSurviveMirrorGC`：上游删分支、镜像 gc 之后会话 `fsck` 和 `gc` 通过。去掉第 5 步的同步，这个测试报 `bad object refs/remotes/origin/feature`；去掉第 3 步的锁，overlay 上 `worktree prune` 之后 b 报 `not a git repository`。
