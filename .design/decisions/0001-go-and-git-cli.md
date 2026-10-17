# 0001 用 Go 实现，git 操作调用命令行

- 状态：已采纳
- 日期：2026-10-17

## 背景

要在 Rust 和 Go 之间选实现语言。调研阶段原本倾向 Rust（gitoxide + fuser）。

## 决定

用 Go。所有 git 操作直接调用 `git` 命令行（`internal/gitx`），不用 go-git 或 libgit2。

## 理由

- 示例项目 tingly-box 本身是 Go，同一生态，可以嵌入或同源发布，单个二进制可交叉编译到 Linux 和 macOS。
- 第一阶段不需要 FUSE：Linux 用内核 overlayfs，macOS 用 APFS clonefile。Rust 在 FUSE 性能上的优势暂时用不上，而 `golang.org/x/sys/unix` 已经覆盖 mount、unshare、clonefile。
- alternates、子模块、fsmonitor 这些行为要和 agent 自己运行的 git 完全一致，命令行最稳。

## 被放弃的选项

- Rust：FUSE 阶段才有明显优势，现在引入的成本大于收益。
- go-git：对 alternates 和子模块的支持与真实 git 有差距。

## 后果

- 将来如果上 FUSE，用 go-fuse；性能不够再单独用 Rust 写那一个守护进程，不影响其余部分。
- 运行环境需要装有 `git`。
