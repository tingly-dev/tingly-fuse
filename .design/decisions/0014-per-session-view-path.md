# 0014 每个会话一个视图路径

- 状态：已采纳，取代 [0002](0002-same-path-private-view.md)
- 日期：2026-10-22

## 背景

[0002](0002-same-path-private-view.md) 让 Linux 上所有会话在同一个路径（`/workspace/<repo>`）下工作，macOS 因为没有 mount namespace 只能每会话一个路径。结果是两个平台的路径模型不同，文档、提示词、测试都要分两套说。YZ 重新考虑这个问题时的倾向是：大家各自有各自的路径，需要的信息另外共享。

这期间上游也变了：[0011](0011-shared-paths.md) 加了任意路径共享，[0010](0010-shared-git-via-registered-worktrees.md) 加了共享 `.git`，[0013](0013-generic-layer-and-git-subcommand.md) 把命令拆成了通用层和 `git` 子命令。共享已经有了显式的通道，不再需要靠"路径相同"来共享缓存。

## 决定

1. **所有后端、所有平台，视图都在 `sessions/<id>/root`。** overlay 和 copy、clone 一样，路径各不相同。
2. **overlay 的隔离机制不变**：每会话一个常驻 mount namespace，`exec` 用 `nsenter` 加入（[0006](0006-overlay-persistent-mount-namespace.md)）。只是挂载点从 `/workspace/<repo>` 换成会话自己的目录。
3. **去掉 `TINGLY_FUSE_VIEW_ROOT` 和 `/workspace`。** 不保留同一路径的可选模式：没人用的选项只会让测试矩阵变大，真需要时再加。已有会话的 `session.json` 里记着旧的视图路径，照旧能用。
4. **按绝对路径命中的缓存用工具自己的选项解决**，例如 Go 的 `-trimpath`。tingly-fuse 不替用户设置这些环境变量（环境准备不归它管，[0004](0004-environment-prep-out-of-scope.md)）。

## 理由

- **路径就是会话的名字。** 日志、报错、人和 agent 的对话里出现一个路径，就能知道是哪个会话。同一路径下 `/workspace/<repo>` 分不清。
- **两个平台一致。** 同一套文档、提示词和测试；macOS 不需要为了路径一致再去做轻量虚拟机。
- **agent 好接。** 路径各不相同的模型和 worktree 一样，agent 框架 cd 进目录就能用。
- **少一个前置步骤。** 非 root 用户不用再先 `sudo mkdir /workspace`。
- **0002 的主要收益可以用别的办法拿到。** 2026-10-22 在 tingly-box 上实测（两份副本放在不同路径，共用 `GOCACHE` 和模块缓存，`go build ./cli/tingly-box`）：

  | | 第一个路径 | 第二个路径 |
  |---|---|---|
  | 不加 `-trimpath` | 181 秒 | 93 秒 |
  | 加 `-trimpath` | 183 秒 | 10 秒 |

  加 `-trimpath` 后第二个路径和 [bench](../../bench/README.md) 里同一路径 overlay 的约 15 秒是同一个量级。
- **Claude Cloud Projects 的做法可以对照。** 每个 thread 是一台独立的 Firecracker 虚拟机，仓库都 clone 在同一个路径，各自一个分支；thread 之间只通过 git 远端、一个共享的文件夹和记忆共享。它能用同一路径，是因为每个 agent 独占一台机器，人也从不直接看 agent 的工作区。tingly-fuse 是人和多个 agent 在同一台机器上，人要看、IDE 要打开，所以路径各不相同更合适；而"共享只走显式通道"这一点和它一致。

## 被放弃的选项

- **保持现状（Linux 同一路径，macOS 不同）。** 平台差异一直存在，所有面向用户的说明都要分两种情况。
- **保留同一路径作为 Linux 上的可选模式。** 要一直维护视图根目录、挂载点权限和对应的测试，目前没有非它不可的场景。
- **macOS 上用轻量 Linux 虚拟机实现同一路径。** 只为保住路径一致，代价太大。

## 后果

- 没有 `-trimpath` 这类选项的工具，跨会话的缓存会失效：C/C++ 需要 ccache 的 `base_dir`，Rust 需要 `--remap-path-prefix`，Python venv 的脚本里写死了绝对路径。环境准备时生成的、带绝对路径的产物，会指向准备时的路径。
- overlay 的视图在宿主上仍然看不到：`sessions/<id>/root` 是空的挂载点，只有会话的 namespace 里有内容。想在宿主上直接用编辑器打开，还要再做一步：root 时直接在宿主上挂 overlay，非 root 时用 fuse-overlayfs 或 reflink 复制，需要实验比较后再定。
- `--shared-git` 的 worktree 仍要加锁（[0010](0010-shared-git-via-registered-worktrees.md)），原因从"路径在宿主上不存在"变成"路径在宿主上是空目录"，效果一样。
- [bench/README.md](../../bench/README.md) 里"同一路径快 10 倍"的结论是 0002 时的数据，保留作历史记录。
