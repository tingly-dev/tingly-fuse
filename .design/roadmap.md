# 路线图

分步推进。每一步有明确范围和验收方式，做完一步再开下一步；一步完成后更新这里的状态，并在 [decisions/](decisions/) 记录途中做的取舍。

状态：✅ 完成　🔶 进行中或待验证　⬜ 未开始　💭 仅调研，未决定做不做

## 已完成

| 步骤 | 范围 | 验收 | 状态 |
|---|---|---|---|
| 1. 核心与接口 | 会话模型、CLI（repo/spawn/exec/push/drop/list）、Backend 接口、copy 基准后端 | 两个会话互相隔离且各自推分支、base 不被修改、8 个会话并发 spawn 只生成一份 base、spawn 失败自动清理；`go test -race` 通过 | ✅ |
| 2. Linux overlay 后端 | mount namespace + overlayfs，同一视图路径 | root 和非 root 下测试通过；tingly-box 上每会话约 100KB | ✅ |
| 2a. overlay 并发 exec | 每会话一个常驻 mount namespace（[0006](decisions/0006-overlay-persistent-mount-namespace.md)） | 并发 `exec` 互相可见且只有一个挂载，内核无告警 | ✅ |
| 3. 基准测试 | worktree / copy / reflink / overlay 在 tingly-box 上的创建、依赖安装、`go build`、`git status`、占用 | 报告和脚本在 [../bench/](../bench/) | ✅ |
| 4. 子模块 | 递归检出子模块，对象走共享镜像（[0003](decisions/0003-shared-objects-via-alternates.md)） | tingly-box 在 overlay 会话里完整 `go build ./...` | ✅ |
| 5. 仓库管理 | `repo remove`、`repo list -v`（PR #2） | 已合并 | ✅ |
| 6. macOS clone 后端 | APFS clonefile，非 APFS 退回复制 | 2026-10-17 在 YZ 的 Mac 上验证：`list` 显示后端为 `clone`；base 已存在时 spawn 0.09 秒（第一次 13 秒，主要是检出 base 和子模块）；`df` 前后对比，新会话只多占约 2 MB（约 5.6k 个 inode），整份复制则约 75 MB；`go test -race ./...` 通过，包括 `TestCloneTreeNative` | ✅ |
| 7. `spawn --from-dir` | 接收已准备好的目录作底层：reflink 快照、并入对象、处理 `.git` 与子模块、`--base` 复用（[0007](decisions/0007-from-dir-snapshot-and-absorb-objects.md)，[issue #1](https://github.com/tingly-dev/tingly-fuse/issues/1)，范围见 [0004](decisions/0004-environment-prep-out-of-scope.md)） | 单元测试覆盖忽略文件、未推送提交、暂存 blob、alternates、子模块、源目录被删；2026-10-17 在真实 tingly-box（含 3 个子模块、frontend 640 MB node_modules）上验证：快照 7 秒；会话里直接跑 tsc 额外约 4 MB；固定 pnpm store 目录后 `pnpm install --frozen-lockfile` 1.2 秒、额外约 12 MB。不固定 store 时 pnpm 因 overlay 上设备号不同而改用项目内 store、要清掉 node_modules 重装，额外约 690 MB（见 [0007](decisions/0007-from-dir-snapshot-and-absorb-objects.md)） | ✅ |

| 8. base 对象可达性与新鲜度 | base、`--from-dir` 并入的对象、子模块 commit 在镜像里建固定 ref；`spawn --from <分支>` 先 fetch；`repo fetch`、`repo check [--fix]`（[0008](decisions/0008-pin-base-objects-in-mirrors.md)，[issue #6](https://github.com/tingly-dev/tingly-fuse/issues/6) 第 1、2 步） | `internal/manager/lifecycle_test.go` 的场景在修复前全部失败（视图里 `git status` 报 `bad object HEAD`），修复后通过：上游删分支并重写 main、`--from-dir` 的未推送提交 / reflog / stash / 暂存 blob、子模块 commit 所在分支被删，之后镜像 fetch --prune 加 `gc --prune=now`，视图里 `git fsck` 无错。2026-10-18 在真实 tingly-box 上：首次固定约 0.5 秒（4 个 gitdir），之后每次 spawn 只多一次 `rev-parse`；固定后 `gc --prune=now`，overlay 和 copy 会话里 `git fsck` 均通过 | ✅ |
| 9. base 回收 | `base list/rm/prune`，按会话引用回收，删除时一并删掉固定 ref，与 spawn 加锁互斥（[0009](decisions/0009-base-removal-by-session-refcount.md)，issue #6 第 3 步） | `TestRemoveAndPruneBases`：被引用的 base 拒绝删除，prune 后固定 ref 消失、gc 回收只属于它的对象、留下的会话 `fsck` 正常；`TestPruneDoesNotRaceSpawn`：去掉锁时稳定失败，加锁后通过。2026-10-18 tingly-box 端到端（[bench/lifecycle.sh](../bench/lifecycle.sh)）：prune 一个 commit base 和一个快照后 gc，仓库目录约 443 MB 降到约 294 MB | ✅ |
| 10. harness | lifecycle 场景测试在每个可用后端上跑；CLI 端到端测试；CI（gofmt、vet、Linux 普通用户和 root、macOS、tingly-box 端到端） | 本地：lifecycle 场景在 copy 和 overlay 上全部通过，以 uid 65534 运行也通过；CLI 测试能抓到之前 `base` 子命令参数解析的 bug（把 bug 放回去时测试失败）。2026-10-18 PR #8 的 CI 全绿：macOS 上 7 个场景在 copy 和 clone 上都执行并通过，ubuntu 上 root 跑 overlay，tingly-box 端到端在 copy 和 overlay 上通过。CI 首跑还暴露了 overlay 测试探测在没有 user namespace 的机器上会空指针，已修 | ✅ |
| 11. 共享 `.git` | `repo add --shared-git`：每个会话是 `shared.git` 的一个已登记、已锁定的 worktree，refs、config、新对象共用；镜像替 shared.git 的 refs 保留对象（[0010](decisions/0010-shared-git-via-registered-worktrees.md)） | `sharedgit_test.go` 在 copy 和 overlay 上通过（root 和 uid 65534）：跨会话可见、不能检出同一分支、一个会话 `gc --prune=now` 后另一个会话暂存的 blob 还在、上游删分支加镜像 gc 后 `fsck` 和 `gc` 正常；分别去掉锁和 ref 同步时对应测试失败 | 🔶 |

## 进行中与待做

| 步骤 | 范围 | 验收 | 状态 |
|---|---|---|---|

## 之后可能做的

这些都来自 [调研](research/research-multi-agent-git-workspace.md)，没有确定要做，要看实际使用中哪些真的有需求。

| 方向 | 要解决什么 | 状态 |
|---|---|---|
| 共享 / 单向可见 / 按目录混合的 IO 策略 | `.git` 已可共享（步骤 11），工作区文件仍完全隔离；紧密协作的 agent 需要实时看到彼此的改动 | 💭 |
| fsmonitor 与变更日志 | 让 `git status` 不扫全盘；给构建工具和 watcher 变更流 | 💭 |
| checkpoint 与 fork | 冻结某个会话的 upper 作为只读层，在其上分叉新会话；或把 upper 写成 commit | 💭 |
| base 生命周期的其余部分 | base 标签、会话迁移到新 base（[issue #6](https://github.com/tingly-dev/tingly-fuse/issues/6) 第 4、5 步） | 💭 |
| 跨会话冲突预警 | 提前发现两个会话改了同一个路径 | 💭 |
| macOS 同一路径（虚拟机模式） | 需要严格路径一致时，在常驻轻量 Linux 虚拟机里跑 overlay | 💭 |
| FUSE 后端 | 按需加载 git 对象，创建会话 O(1)；是否值得取决于基准里 copy / overlay 之外还差什么 | 💭 |

## 开放问题

- clone 后端在 macOS 上如何处理路径不同对构建缓存的影响？目前的缓解办法是脚本读 `TINGLY_WORKSPACE`、Go 构建加 `-trimpath`。
- overlay 的视图根目录默认 `/workspace`，非 root 用户需要先建好挂载点或改用 `TINGLY_FUSE_VIEW_ROOT`，是否要换成更省事的默认值？
