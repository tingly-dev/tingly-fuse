# 0007 `spawn --from-dir`：快照已准备好的目录，并把其中的对象并入共享镜像

- 状态：已采纳
- 日期：2026-10-17

## 背景

[0004](0004-environment-prep-out-of-scope.md) 规定环境准备不归 tingly-fuse 管：依赖安装、构建发生在它之前。但这样得有办法把一个现成的目录当作底层交给它，否则每个会话的 overlay 下仍要重新装依赖（pnpm 在 overlay 下无法跨挂载点硬链接，每会话约 800 MB，见 [issue #1](https://github.com/tingly-dev/tingly-fuse/issues/1)）。

## 决定

`spawn <repo> --from-dir <dir>` 把 `<dir>` 作为只读底层，具体做法：

1. **快照而不是引用。** 用 reflink 复制（Linux `cp -a --reflink=auto`，macOS `cp -cRp`）到 `bases/dir-<head前12位>-<UTC时间>/`。源目录之后随便改、甚至删掉，都不影响已有会话。文件系统不支持 reflink 时退回整份复制。
2. **目录内容按原样取用，包括未提交的改动和 ignore 掉的文件。** 这正是目的：`node_modules`、构建产物都在里面。
3. **把快照里的对象并入共享镜像。** 目录里可能有从未推送的提交、已暂存的 blob，丢了会让视图里的 git 坏掉。对象来源包括快照自己的 `.git/objects`，以及它的 `objects/info/alternates` 递归指向的目录（后者只读取、复制进镜像，不移动）。已存在的对象跳过；pack 先放 `.pack` 再放 `.idx`。然后把快照的 `.git/objects` 清空，只留指向镜像的 alternates，和 [0003](0003-shared-objects-via-alternates.md) 里 `EnsureBase` 产出的形态一致。
4. **子模块。** `.git/modules/**` 下每个 gitdir 按它的 `remote.origin.url` 并入对应的子模块镜像（没有则新建一个空的 bare 镜像，fetch refspec 为 `+refs/*:refs/*`，之后 `ensureModuleMirror` 需要时照常 fetch）。取不到 URL 的 gitdir 保持原样。
5. **origin 改成 upstream**，保证 `push` 推到真实远端（[0005](0005-merge-on-remote.md)）。
6. **同一份快照多会话共用：** `spawn --base <id>`，id 在 `repo list -v` 里看。`--from`、`--from-dir`、`--base` 互斥。
7. 会话记录里 `base` 是 base id，另存 `head`（快照所在的 commit）和 `source`（源目录路径）。

## 被放弃的选项

- **直接把源目录当 overlay 的 lower，不复制。** 源目录会被使用者继续改，lower 在挂载期间被改动会导致 overlayfs 行为未定义；快照才能保证只读。
- **只认 commit，丢掉目录里额外的对象和未提交内容。** 那样等于重新 checkout，依赖和构建产物都没了，失去意义。
- **支持 `.git` 是文件的 worktree 或 submodule 目录。** gitdir 在目录之外，快照复制不到，先要求 `.git` 是目录，报错信息里说明。

## 后果

- 快照在不支持 reflink 的文件系统上会整份复制，创建慢、占空间多；之后的会话仍然是 CoW。
- 并入镜像的对象不会自动清理，`repo remove` 才会一并删除。
- 每次 `--from-dir` 都生成新 base，旧快照不会自动删除。
- 镜像里可能因此含有上游没有的对象（未推送提交、暂存的 blob），不影响 `fetch`。
