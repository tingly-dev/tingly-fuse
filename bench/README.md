# 工作区方案基准测试

在 [tingly-dev/tingly-box](https://github.com/tingly-dev/tingly-box)（commit `4819d30`，约 3.2k 个文件，含 3 个子模块）上，对比给每个 agent 开一个私有工作区的四种做法：

| 方案 | 做法 | 视图路径 |
|---|---|---|
| worktree | 一个主 clone，每会话 `git worktree add` + `git submodule update --init` | 每会话不同 |
| copy | tingly-fuse `copy` 后端（`cp -a` 基础 checkout） | 每会话不同 |
| reflink | 同 copy，但 `cp -a --reflink=always`（Linux 上相当于 APFS clonefile） | 每会话不同 |
| overlay | tingly-fuse `overlay` 后端（6996d6f，常驻 mount namespace + overlayfs） | 所有会话都是 `/workspace/tingly-box` |

脚本：[bench.sh](bench.sh)。原始数据：[results/xfs.csv](results/xfs.csv)、[results/ext4.csv](results/ext4.csv)，环境见 [results/env.txt](results/env.txt)。

## 结论

1. **建工作区本身不是瓶颈。** 源码树只有约 74 MB，worktree 和 copy 都在 1 秒内建好。overlay 约 30 ms、几乎不占盘；reflink 文件系统上 copy 也几乎不占盘（GNU cp 9.x 的 `cp -a` 默认就会尝试 reflink，所以 XFS 上 copy 和 reflink 结果一样）。
2. **真正的大头是 Go 构建，而 Go 构建缓存按绝对路径命中。** 共享同一个 `GOCACHE` 的情况下，在一个新路径上构建 tingly-box 要约 150 秒；worktree、copy、reflink 每个会话路径都不同，所以每个会话都要付这 150 秒。overlay 所有会话同一路径，第二个会话起只要约 15 秒，快 10 倍。这是"同一路径"设计最直接的收益。
3. **overlay 下 pnpm install 会退化。** pnpm 靠硬链接从 store 复用文件，但 overlay 挂载点和 store 不在同一个设备上，硬链接失败后整份复制：每会话多占约 790–820 MB，耗时也从 6 秒涨到 17–21 秒（ext4）。worktree 和 copy 每会话只多 26 MB（ext4）。
4. **解决办法是把依赖装进基础层。** 基础 checkout 预装 `node_modules` 后，overlay 会话创建仍是 30 ms、0 MB，pnpm install 只做校验（2.4 秒，不到 1 MB）。同样的预热底层对 copy/reflink 反而有代价：每会话创建变成 8–12 秒、67 MB（node_modules 约 11 万个文件，每个都要复制元数据）。
5. **git status 不是区分点。** 这个规模下各方案都在 0.5 秒内；worktree 首次稍慢（0.35–0.47 秒，刷新新 index），之后都约 50 ms。

对 tingly-fuse 的建议：

- overlay 后端配一个"按 commit 预热基础层"的步骤（例如 `pnpm install`、`go mod download`），否则 Node 项目在 overlay 下比 worktree 更费盘。
- **tingly-fuse 目前不初始化子模块。** tingly-box 的 `go.mod` 用 `replace` 指向 `libs/` 下的子模块，不初始化就无法 `go build`。基准里是在基础 checkout 上手动 `git submodule update --init`，再把子模块对象换成指向镜像的 alternates。建议 `EnsureBase` 直接支持。
- 如果要留 worktree/copy 方案，可让 agent 用 `go build -trimpath`：路径不进缓存键，换路径后第二次构建 16.8 秒（但第一次要全冷编译，285 秒）。

## 数据

所有数字是单次测量，4 核、16 GB 内存的云容器，内核 6.18。pnpm store、Go 模块缓存和构建缓存都已预热（`--offline` 安装，不走网络）。磁盘是专用回环文件系统上 statfs 已用量的增量，硬链接、reflink 共享块和 overlay 差量层都按实际占用计算。每种方案建 3 个会话，在前 2 个上跑 pnpm install 和 go build。

### 干净底层，ext4（无 reflink）

| 方案 | 创建（每会话） | pnpm install | go build 会话1 / 会话2 | 首次 git status | 3 会话总占用 |
|---|---|---|---|---|---|
| worktree | 0.83 s, 74 MB | 6.0 s, 26 MB | 149 s / 147 s | 0.35 s | 275 MB |
| copy | 0.29 s, 74 MB | 5.8–6.3 s, 26 MB | 150 s / 153 s | 0.06 s | 275 MB |
| overlay | 0.03 s, 0.1 MB | 17–22 s, 786 MB | 15.5 s / 17.0 s ※ | 0.08–0.10 s | 1573 MB |

※ 此前 XFS 那轮已在 `/workspace/tingly-box` 构建过，缓存按路径命中，所以 ext4 这轮的会话 1 也是热的。

### 干净底层，XFS（支持 reflink）

| 方案 | 创建（每会话） | pnpm install | go build 会话1 / 会话2 | 首次 git status | 3 会话总占用 |
|---|---|---|---|---|---|
| worktree | 0.79–0.88 s, 72–75 MB | 10.2–13.5 s, 65 MB | 156 s / 155 s | 0.39–0.47 s | 350 MB |
| copy | 0.29–0.42 s, 1–3 MB | 10.8–12.3 s, 65 MB | 150 s / 153 s | 0.06–0.09 s | 136 MB |
| reflink | 0.29–0.39 s, 1–3 MB | 10.2–10.6 s, 65 MB | 157 s / 158 s | 0.06 s | 136 MB |
| overlay | 0.03 s, 0 MB | 10.7–19.6 s, 822 MB | **157 s / 15.3 s** | 0.21 s | 1646 MB |

overlay 这一行是"同一路径"的干净对照：会话 1 是这个路径第一次构建，会话 2 直接命中会话 1 留下的缓存。

### 预热底层（基础层预装 node_modules），XFS

| 方案 | 创建（每会话） | pnpm install | 3 会话总占用 |
|---|---|---|---|
| copy | 11.4–12.0 s, 67 MB | 2.3–2.6 s, 0.4 MB | 203 MB |
| reflink | 8.1–11.1 s, 67 MB | 2.3–2.4 s, 0.4 MB | 202 MB |
| overlay | 0.03 s, 0 MB | 2.4 s, 0.7 MB | 1.5 MB |

这一轮 copy/reflink 的会话路径和上一轮相同，所以 go build 也命中了缓存（15–23 秒），再次说明命中只看路径；表里不列。

## 怎么复跑

需要 Linux root（overlay 要建 mount namespace，基准要挂回环盘）。

```sh
# 一块支持 reflink 的 XFS 回环盘（只测 ext4 可跳过，改 BENCH_FS）
truncate -s 20G xfs.img && mkfs.xfs -m reflink=1 xfs.img
mkdir -p /mnt/xfs && mount -o loop xfs.img /mnt/xfs

go build -o /usr/local/bin/tingly-fuse ./cmd/tingly-fuse
bench/bench.sh setup                           # 镜像、子模块、预热缓存，需联网
bench/bench.sh run                             # 结果在 ./bench-out/results.csv
BENCH_WARM_BASE=1 BENCH_METHODS="copy reflink overlay" bench/bench.sh run
```

注意：Go 缓存按路径命中，复跑时会话路径和上一轮相同就会直接命中。要测"新路径冷构建"，先换 `BENCH_FS` 或清掉 `GOCACHE` 里的对应条目（最简单是 `go clean -cache`，但之后要重新预热依赖）。

## base 生命周期（[lifecycle.sh](lifecycle.sh)，[决策 0008](../.design/decisions/0008-pin-base-objects-in-mirrors.md)）

端到端验证 base 在上游改写历史、镜像 gc 之后是否还能用。脚本把仓库 `--mirror` 到本地当 upstream，建三个会话（`--from main`、`--from topic`、`--from-dir` 一个含未推送提交、stash、暂存文件的目录），然后删掉 topic、force-push 改写 main，对所有镜像 `fetch --prune` 加 `gc --prune=now`，最后在每个会话里跑 `git status` 和 `git fsck`（含子模块）。`TF_OLD` 可以指定旧版本，用它建 base、再用当前版本 `repo check --fix`，验证升级路径。

2026-10-18，tingly-box（`5f83f04`，3 个子模块），Linux 云容器：

| 运行 | 结果 |
|---|---|
| 对照：PR #7 之前的版本，不修复（`FIX=0`） | topic 和 `--from-dir` 会话 `fatal: bad object HEAD`；main 会话 `fsck` 报 `refs/remotes/origin/topic` 指向缺失对象；`spawn --base` 复用旧 topic base 失败 |
| 升级：旧版本建 base，新版本 `repo check --fix`，overlay 后端 | 修复前 3 个 base 都报未固定；`--fix` 1.4 秒；gc 后三个会话 `fsck` 全部通过，`spawn --base` 和 `--from main`（拿到改写后的 main）都正常；镜像里共 40 个固定 ref |
| 只用新版本，copy 后端 | 同上，全部通过 |

回收：最后 drop 掉除 main 外的会话，`base prune` 删掉 topic 的 base 和 `--from-dir` 快照，再 gc，仓库目录从约 443 MB 降到约 294 MB，main 会话 `fsck` 仍然通过。最初版本的脚本还发现 `base` 子命令参数解析有错，单元测试没覆盖到 CLI。

固定 ref 的数量：最初每个 ref 单独复制一份，3 个 base 在镜像里留下约 5500 个 ref（tingly-box 每个 gitdir 带约 900 个远端跟踪分支和 tag）；改成把指向 commit 的 ref 收进 keep commit 的 parent 之后是 40 个。
