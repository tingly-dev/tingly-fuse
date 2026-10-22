# 让 overlay 视图在宿主上可见（[hostview.sh](hostview.sh)）

问题（[issue #12](https://github.com/tingly-dev/tingly-fuse/issues/12)）：非 root 用户想让会话视图在宿主上可见，用 fuse-overlayfs 还是复制（XFS 上 reflink）？内核 overlay 直接挂在宿主上（需要 root）作基准。

2026-10-22，tingly-box `e983a20`（含子模块），4 核、16 GB 云虚拟机，内核 6.18，fuse-overlayfs 1.13，每种做法 3 个会话取平均，单次运行。回环盘上的 ext4 和 XFS（`reflink=1`）。依赖（frontend node_modules）预装进 base（`--from-dir`），Go 构建缓存预热并共用（`-trimpath`）。原始数据：[results/hostview.csv](results/hostview.csv)，环境：[results/hostview-env.txt](results/hostview-env.txt)。

| 文件系统 | 做法 | 创建 | 占盘 | 宿主 `git status` | 会话里 `git status` | `pnpm install` | vite build | go build |
|---|---|---|---|---|---|---|---|---|
| ext4 | copy | 3.14 s | 724 MB | 0.03 s | 0.03 s | 1.8 s | 3.5 s | 12.4 s |
| ext4 | overlay（root） | 0.02 s | 0.1 MB | 0.05 s | 0.04 s | 1.7 s | 3.7 s | 10.9 s |
| ext4 | fuse-overlay | 0.02 s | 0.1 MB | 0.28 s | 0.08 s | 11.1 s | 4.6 s | 11.3 s |
| XFS | copy（reflink） | 6.09 s | 64 MB | 0.04 s | 0.03 s | 1.7 s | 3.7 s | 13.2 s |
| XFS | overlay（root） | 0.02 s | 0.1 MB | 0.05 s | 0.04 s | 1.7 s | 4.0 s | 13.5 s |
| XFS | fuse-overlay | 0.02 s | 0.1 MB | 0.29 s | 0.08 s | 2.2 s | 4.7 s | 14.2 s |

要点：

- fuse-overlay 创建几乎免费、不占盘；构建比内核 overlay 慢约 0%–25%；宿主上扫工作区（IDE 打开）约 0.3 s。
- copy 在 ext4 上是真复制（724 MB）；XFS 上 `cp -a` 自动 reflink，占盘降到 64 MB，但创建比 ext4 上还慢（逐文件 reflink）。
- ext4 上 fuse-overlay 的 `pnpm install` 比其他慢很多（11 s vs 1.7 s），XFS 上没有这个现象；没查原因。
- 磁盘用量是 `statfs` 增量，包含 git 元数据和构建产物（vite 约 5 MB、go build 约 3 MB）。

复现：`TINGLY_FUSE=$(which tingly-fuse) bench/hostview.sh ext4 xfs`（root，需要 fuse-overlayfs、pnpm、go、mkfs.xfs）。
