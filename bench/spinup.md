# 从零开 N 个工作区并构建（[spinup.sh](spinup.sh)）

问题：用 tingly-fuse 现有的功能给多个 agent 各开一个工作区、装好依赖、构建起来，和直接 `git clone` 或 `git worktree` 相比差多少。和 [README.md](README.md) 里的对比不同，这里**一次性成本也算在内**：每种做法都从空目录开始。

2026-10-22，tingly-box `00b45f9`（含子模块），4 核、16 GB 的云虚拟机，内核 6.18，专用 ext4 回环盘（没有 reflink），单次测量。原始数据：[results/spinup-ext4.csv](results/spinup-ext4.csv)，环境：[results/spinup-env.txt](results/spinup-env.txt)。

## 做法

| 做法 | 一次性准备 | 每个 agent |
|---|---|---|
| clone | 无 | `git clone --recurse-submodules`，走网络 |
| worktree | 从上游 clone 一个主仓库 | `git worktree add`，再 `git submodule update --init`（子模块走网络） |
| tf | `tingly-fuse git clone` | `git worktree add`（overlay） |
| tf-share | 同 tf，加 `--share frontend/node_modules` | 同 tf |
| tf-fromdir | 普通 clone 一份，`pnpm install`，用 `--from-dir` 做成 base，然后删掉这份 clone | `git worktree add --base <id>` |

每个工作区接着跑 `git status`、`pnpm install --offline`（store 已预热，和工作区在同一块盘上）、`go build -trimpath ./cli/tingly-box`（所有做法共用一个 `GOCACHE`，每种做法开始前清空；模块缓存已预热）。所有做法的路径都各不相同（[0014](../.design/decisions/0014-per-session-view-path.md)），所以都加了 `-trimpath`；不加的话，每个新路径都要多花约 80 秒，各做法一样。

## 结果

第一个工作区包含一次性准备。增量是第 2、3 个工作区的平均值。

| 做法 | 第一个工作区 | 每多一个：创建 | 每多一个：pnpm | 每多一个：go build | 每多一个：合计 | 每多一个：磁盘 | 3 个总计 |
|---|---|---|---|---|---|---|---|
| clone | 200 s, 264 MB | 16.4 s | 3.9 s | 10.2 s | **30.7 s** | **263 MB** | 262 s, 791 MB |
| worktree | 203 s, 384 MB | 7.8 s | 3.5 s | 9.5 s | **21.0 s** | **144 MB** | 246 s, 672 MB |
| tf | 204 s, 1023 MB | 0.6 s | 8.5 s | 11.3 s | **20.8 s** | **787 MB** | 246 s, 2596 MB |
| tf-share | 196 s, 1026 MB | 0.7 s | 1.4 s | 9.6 s | **11.9 s** | **0.8 MB** | 220 s, 1028 MB |
| tf-fromdir | 201 s, 891 MB | 0.02 s | 1.3 s | 9.4 s | **11.0 s** | **12 MB** | 224 s, 915 MB |

按这个增量外推到 10 个 agent：clone 约 475 秒、2.6 GB；worktree 约 390 秒、1.7 GB；tf 约 390 秒、8.1 GB；tf-share 和 tf-fromdir 约 300 秒、1.0 GB。

## 结论

1. **第一个工作区各做法都一样，约 200 秒，几乎全是冷的 `go build`（约 180 秒）。** 只开一两个工作区时，tingly-fuse 没有明显优势。
2. **优势在后面每多开一个。** 带上依赖的做法（tf-share、tf-fromdir），每多一个 agent 约 11 秒、几 MB：比完整 clone 快约 3 倍、省约 250 MB；比 worktree 快约 2 倍、省约 140 MB。创建本身从 8–16 秒降到 0.02–0.7 秒。
3. **直接用 overlay、不处理 node_modules 是最差的。** pnpm 在 overlay 里没法硬链接到 store（不同设备），每个工作区整份复制约 790 MB，10 个 agent 要 8 GB。要么用 `--share frontend/node_modules`，要么用 `--from-dir` 把装好的依赖放进 base。
4. **剩下的大头是构建本身。** 每多一个工作区，`go build` 在缓存全热的情况下仍要约 10 秒（链接等），占增量的 80% 以上；第一个工作区的 180 秒冷构建是总时间的大头。git 和文件这一层已经不是瓶颈，再往下要做的是构建产物、缓存的共享。

## 说明

- `tf` 的创建 0.6 秒，差别多半来自 `git worktree add` 默认先 fetch 上游（tf-fromdir 用 `--base`，不 fetch，只要 0.02 秒）；没有单独测 `--no-fetch`。
- tf-share 第一个工作区的 pnpm 仍整份复制了 786 MB：共享目录是 bind mount，跨挂载点同样不能硬链接。之后的工作区直接复用这一份。
- tf-share 的共享 `node_modules` 是所有会话写同一份，适合依赖相同的 agent；依赖要改时用 tf-fromdir 更稳。
- tf-fromdir 的一次性准备（28 秒、879 MB）包括 clone、`pnpm install` 和快照；base 里的 node_modules 是整份复制的（ext4 没有 reflink），在 XFS、btrfs 或 APFS 上会小很多。
- clone 和 worktree 的创建时间包含网络（clone 整个仓库；worktree 拉子模块），波动较大。

## 复跑

```sh
truncate -s 16G bench.img && mkfs.ext4 -q bench.img && mkdir -p /mnt/bench && mount -o loop bench.img /mnt/bench
go build -o /usr/local/bin/tingly-fuse ./cmd/tingly-fuse
bench/spinup.sh setup && bench/spinup.sh run    # 结果在 ./bench-out/spinup.csv
```
