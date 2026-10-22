# 从零开 N 个工作区（[spinup.sh](spinup.sh)）

问题：用 tingly-fuse 现有的功能给多个 agent 各开一个工作区，和直接 `git clone`、`git worktree` 比差多少。**一次性成本也算在内**，每组都从空目录开始。工作区做法和依赖方式分开测，每种组合跑 3 个工作区。

2026-10-22，tingly-box `374ad23`（含子模块），4 核、16 GB 云虚拟机，内核 6.18，专用 ext4 回环盘（没有 reflink），每组单次测量。原始数据：[results/spinup-deps-ext4.csv](results/spinup-deps-ext4.csv)，环境：[results/spinup-env.txt](results/spinup-env.txt)。

## 测了什么

工作区做法：

| 做法 | 一次性准备 | 每个 agent |
|---|---|---|
| clone | 无 | `git clone --recurse-submodules`，走网络 |
| worktree | 从上游 clone 一个主仓库 | `git worktree add`，再 `git submodule update --init`（子模块走网络） |
| tf | `tingly-fuse git clone` | `git worktree add`（overlay） |

依赖方式（frontend，pnpm）：

| 依赖 | 做法 |
|---|---|
| 独立 store | 每个工作区自己的 pnpm store，联网安装，什么都不共享 |
| 共享 store | 共用一个预热好的 store，各自一份 node_modules（离线安装） |
| 共享 node_modules | `frontend/node_modules` 所有工作区共用一份：clone、worktree 用 bind mount，tf 用 `--share`，机制相同 |
| 预装进 base | 只有 tf：普通 clone 一份装好依赖，`--from-dir` 做成只读 base |

每个工作区依次：创建、`git status`、`pnpm install`、`pnpm build`（vite）。

## 结果

每多开一个工作区（第 2、3 个的平均），按步骤拆开：仓库（创建工作区，不含依赖）、依赖（`pnpm install`）、构建（`pnpm build`）。

| 组合 | 仓库 | 依赖 | 构建 | 合计 |
|---|---|---|---|---|
| clone + 共享 store | 16.7 s, 235 MB | 5.3 s, 26 MB | 7.1 s, 5 MB | 29.6 s, 267 MB |
| worktree + 共享 store | 8.8 s, 117 MB | 5.8 s, 26 MB | 7.3 s, 5 MB | 22.3 s, 149 MB |
| tf + 共享 store | 0.7 s, 0.1 MB | 15.2 s, 786 MB | 7.0 s, 5 MB | 23.4 s, 792 MB |
| clone + 共享 node_modules | 17.8 s, 235 MB | 2.2 s, 0.2 MB | 6.5 s, 5 MB | 26.9 s, 241 MB |
| worktree + 共享 node_modules | 9.1 s, 117 MB | 2.1 s, 0.2 MB | 6.7 s, 5 MB | 18.2 s, 122 MB |
| tf + 共享 node_modules | 0.6 s, 0.1 MB | 2.2 s, 0.2 MB | 5.1 s, 5 MB | 8.3 s, 6 MB |
| tf + 预装进 base | 0.02 s, 0.1 MB | 2.4 s, 12 MB | 5.7 s, 5 MB | 8.6 s, 18 MB |
| clone + 独立 store | 18.8 s, 236 MB | 15.2 s, 679 MB | 6.2 s, 5 MB | 40.5 s, 920 MB |
| worktree + 独立 store | 8.4 s, 118 MB | 13.0 s, 679 MB | 6.5 s, 5 MB | 28.2 s, 802 MB |
| tf + 独立 store | 0.8 s, 0.1 MB | 31.2 s, 1428 MB | 6.9 s, 5 MB | 39.2 s, 1434 MB |

只看仓库这一步，tf 每个工作区 0.1 MB、不到 1 秒；worktree 117 MB、约 9 秒；clone 235 MB、约 17 秒。tf 的差距全在依赖这一步：pnpm 在 overlay 里不能硬链接到 store。

合计：

| 依赖 \ 做法 | clone | worktree | tf |
|---|---|---|---|
| 独立 store | 40.5 s, 920 MB | 28.2 s, 802 MB | 39.2 s, 1434 MB |
| 共享 store | 29.6 s, 267 MB | 22.3 s, 149 MB | 23.4 s, 792 MB |
| 共享 node_modules | 26.9 s, 241 MB | 18.2 s, 122 MB | **8.3 s, 6 MB** |
| 预装进 base | — | — | **8.6 s, 18 MB** |

拆开看（每多一个，秒）：

| 组合 | 创建 | pnpm install | 前端构建 |
|---|---|---|---|
| clone + 共享 store | 16.7 | 5.3 | 7.1 |
| worktree + 共享 store | 8.8 | 5.8 | 7.3 |
| worktree + 共享 node_modules | 9.1 | 2.1 | 6.7 |
| tf + 共享 store | 0.7 | 15.2 | 7.0 |
| tf + 共享 node_modules | 0.6 | 2.2 | 5.1 |
| tf + 预装进 base | 0.02 | 2.4 | 5.7 |

3 个工作区合计（含一次性准备）：

| 依赖 \ 做法 | clone | worktree | tf |
|---|---|---|---|
| 独立 store | 142 s, 2760 MB | 103 s, 2641 MB | 127 s, 4539 MB |
| 共享 store | 93 s, 801 MB | 82 s, 684 MB | 92 s, 2615 MB |
| 共享 node_modules | 92 s, 1509 MB | 78 s, 1391 MB | 55 s, 1041 MB |
| 预装进 base | — | — | 63 s, 929 MB |

## 结论

1. **tingly-fuse 配上共享 node_modules 或预装进 base，每多一个工作区约 8 秒、几 MB。** worktree 最好也要 18 秒、122 MB，clone 要 27 秒以上、240 MB 以上。省下的主要是创建（worktree 9 秒、clone 17 秒，tf 不到 1 秒）和源码树的磁盘（每份约 120–240 MB）。
2. **tf 配"共享 store"反而最费盘（每个 792 MB）。** pnpm 用 store 的方式是把文件硬链接进 node_modules；overlay 里的工作区和外面的 store 不在同一个挂载点，Linux 不允许跨挂载点硬链接，pnpm 就退回整份复制。clone 和 worktree 是普通目录，硬链接能用，所以只多 26 MB 左右。所以在 tf 下，依赖要么共享 node_modules，要么预装进 base，不能只靠共享 store。
3. **共享 node_modules 时第一个工作区也要整份复制一次**（bind mount 同样跨挂载点），之后的工作区直接复用。clone 和 worktree 用 bind mount 共享时也一样，所以它们这一行的合计比"共享 store"还大。
4. **前端构建各组都是 5–8 秒，依赖怎么放对构建速度没有明显影响。** vite build 没有跨次构建的缓存，"共享构建缓存"对它不适用。
5. 只开 1 个工作区时差别不大；差别随工作区数量线性增长。

## Go 构建（只跑了一部分，按要求停了）

[results/spinup-gobuild-partial-ext4.csv](results/spinup-gobuild-partial-ext4.csv)：每个工作区创建后 `go build ./cli/tingly-box`。

| 构建缓存 | clone | worktree | tf |
|---|---|---|---|
| 全私有（模块缓存和构建缓存各一份） | 286 s, 3.3 GB | 284 s, 3.3 GB | 282 s, 3.3 GB |
| 共享模块缓存，构建缓存各一份 | 274 s, 2.1 GB | 273 s, 2.2 GB | 273 s, 2.1 GB |
| 都共享，不加 `-trimpath` | 152 s, 1.4 GB | 未跑 | 未跑 |

（每多一个工作区的 go build 耗时和磁盘。）Go 构建和工作区做法无关，只看缓存：不共享时每个工作区都是约 4.5 分钟的冷构建。都共享并加 `-trimpath` 的那组没跑完；本仓库 [ADR 0014](../.design/decisions/0014-per-session-view-path.md) 里单独测过，第二个路径约 10 秒。

## 复跑

```sh
truncate -s 16G bench.img && mkfs.ext4 -q bench.img && mkdir -p /mnt/bench && mount -o loop bench.img /mnt/bench
go build -o /usr/local/bin/tingly-fuse ./cmd/tingly-fuse
bench/spinup.sh setup && bench/spinup.sh deps    # 结果在 ./bench-out/spinup.csv；bench/spinup.sh build 测 Go 构建（很慢）
```

脚本可以续跑：已完成的组合会跳过。
