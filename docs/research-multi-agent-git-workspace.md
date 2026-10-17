# 多 Agent 共享一个 Git 仓库：行业方案调研与 tingly-fuse 设计思路

> 状态：调研 / 设计草案（不含实现）　日期：2026-10-17

## 0. 结论先行

1. **冲突的本质不是"同一个仓库"，而是"同一个工作区"。** Git 的对象库（objects、packs）是不可变、可并发写的，天然可以被多个 agent 共享；真正需要隔离的是每个 agent 的 **工作区文件 + index + HEAD**，以及构建产物、依赖目录这类派生状态。
2. **FUSE 和 Docker overlay 不是二选一。** overlay 是一种语义（只读底层 + 可写差量层 + whiteout 删除标记），FUSE 是一种实现手段。业界最成熟的大仓方案（Google CitC、Meta EdenFS）本质上就是"**以版本库为底层、以差量为上层的虚拟文件系统**"。tingly-fuse 最合理的定位是：**git 对象驱动的、按需加载的、每 agent 一个 CoW 差量层的 overlay 文件系统**。
3. **只做文件系统不够，必须同时管好 git 元数据。** EdenFS 能好用的关键之一，是文件系统自己知道哪些文件被改过，从而让 `status`/`diff` 不必全量扫描。tingly-fuse 需要为每个 agent 提供独立的 HEAD/index，并接上 git 的 fsmonitor 接口。
4. **建议分阶段落地：** 先用"共享对象库 + 轻量工作区 + reflink/overlayfs"做一个零 FUSE 的基线（几天量级可验证），再上 FUSE 版本解决懒加载、checkpoint、冲突预警等基线做不到的事。这样 FUSE 的收益可以被量化，而不是凭感觉。

---

## 1. 问题拆解

多个 agent 同时在一个仓库上工作，会撞到下面几类共享状态：

| 状态 | 共用一个 checkout + 切 branch 时的后果 | 能否共享 |
|---|---|---|
| 对象库 `.git/objects` | 无问题，git 对象不可变，写入有原子性 | ✅ 可以完全共享 |
| refs（分支、tag） | 各自建分支不冲突；同名分支需约定 | ✅ 共享，按命名空间区分 |
| HEAD / index | 一个 agent `checkout` 会把另一个 agent 的工作区整体换掉 | ❌ 必须每 agent 一份 |
| 工作区文件 | 互相覆盖，正是用户描述的问题 | ❌ 必须每 agent 一份（但可以 CoW 共享未改动部分） |
| 依赖目录（node_modules、.venv、target） | 版本不同则互相破坏 | ⚠️ 只读部分可共享，写入需隔离 |
| 运行时（端口、数据库、dev server） | 端口冲突、数据串 | ⚠️ 不属于文件系统问题，需要容器/命名空间 |

**worktree 的痛点**（也是用户想避开的）：

- 每个 worktree 都是全量物化的文件，大仓下磁盘和创建时间线性增长；
- 每个 worktree 都要重新装依赖、冷启动构建缓存；
- 同一分支不能被两个 worktree 同时 checkout；
- 数量多了之后的生命周期管理（清理、prune、孤儿目录）麻烦。

所以目标可以精确表述为：**N 个 agent 各自拥有"看起来完整"的工作区，但创建是 O(1)，存储只与改动量成正比，未改动的文件和依赖层共享。**

---

## 2. 行业方案全景

### 2.1 Git 原生能力（不需要新文件系统）

| 方案 | 做法 | 解决了什么 | 没解决什么 |
|---|---|---|---|
| `git worktree` | 共享对象库，每个 worktree 独立 HEAD/index/工作区 | 元数据隔离 | 工作区全量物化；依赖要重装 |
| `clone --shared` / `--reference` / alternates | 多个克隆指向同一对象库 | 对象不重复存储 | 同上 |
| partial clone（`--filter=blob:none`） | 只拉 commit/tree，blob 按需取 | 首次克隆快、省空间 | checkout 时仍然要物化文件 |
| sparse-checkout（cone 模式） | 只物化部分目录 | 大仓中只关心子目录时很有效 | agent 需要预先知道要碰哪些目录 |
| Scalar（已并入 git 2.38+） | 上面几项 + fsmonitor + 后台维护的组合配置 | 大仓日常性能 | 不是多工作区方案 |
| 环境变量 `GIT_DIR` / `GIT_INDEX_FILE` / `GIT_WORK_TREE` | 一个仓库挂多套 index/工作区 | 证明了"多 index 共享对象库"在 git 层面完全可行 | 需要外部工具编排 |

**要点：** git 本身已经把"对象库"和"工作区状态"分离了，tingly-fuse 不需要改 git，只需要接管"工作区文件"这一层。

### 2.2 大仓虚拟文件系统（FUSE / 投影文件系统）

| 方案 | 机构 | 底层机制 | 核心思路 | 现状 |
|---|---|---|---|---|
| **Piper + CitC** | Google | 云端文件系统 | 工作区是云端的"客户端视图"：底层是某个版本的仓库快照，只存储用户改动的文件；工作区可随时快照，任意机器可访问 | 内部系统，有论文/演讲公开 |
| **Sapling + EdenFS** | Meta | Linux FUSE、macOS NFSv3 本地服务、Windows ProjFS | 文件按需从对象库加载；EdenFS 记录 overlay（被修改的文件）和 journal（变更日志），`status` 直接查 overlay 不扫盘 | 已开源 |
| **VFS for Git（GVFS）** | Microsoft | Windows ProjFS | 文件被访问时才"投影"出来 | 已进入维护模式，微软转向 Scalar |
| **bb-clientd / Buildbarn、BuildBuddy 等** | 构建系统社区 | FUSE / NFS | 把 CAS（内容寻址存储）挂成目录，远程执行按需取输入 | 开源 |

**值得借鉴的设计：**

- **按需加载（lazy hydration）**：目录项来自 git tree，文件内容在首次 `open/read` 时才从 blob 取。创建一个工作区只需要记住"base commit"一个 OID。
- **overlay 记录被修改的文件**：EdenFS 内部就是一个"git tree 底层 + 本地差量层"的结构，和 Docker overlay 语义几乎一致。
- **文件系统即变更来源**：EdenFS 的 journal 让 `status`、watchman、构建系统都能直接问"自上次以来改了什么"，不需要 stat 全部文件。
- **跨平台代价很高**：Meta 在 macOS 上从 FUSE 转到 NFS，Windows 用 ProjFS，三套后端。tingly-fuse 应先只做 Linux（agent 多数跑在 Linux 容器里）。

### 2.3 Overlay / 联合文件系统（Docker 路线）

| 方案 | 特点 |
|---|---|
| **overlayfs**（内核） | lowerdir（可多层，只读）+ upperdir（可写）+ workdir；删除用 whiteout，改文件触发整文件 copy-up。Linux 5.11+ 支持在 user namespace 内免 root 挂载 |
| **fuse-overlayfs** | 用户态实现，rootless Podman 早期依赖它；性能低于内核版 |
| **unionfs-fuse / mergerfs** | 更早的联合挂载实现 |
| **Docker / containerd 镜像层** | 每层是一个只读 lower，容器是最上面的 upper；`docker commit` = 把 upper 冻结为新层 |
| **containerd snapshotter**（overlayfs、btrfs、zfs、stargz） | 抽象了"从父快照派生可写快照"的接口；stargz 支持镜像层懒加载 |

**值得借鉴：**

- **层可以叠**：base 代码层 / 预装依赖层 / agent 差量层，三层结构天然解决"依赖不想重装"。
- **commit 一个 upper 就是一个新层**：这直接对应 agent 的 checkpoint，和 fork（从某 agent 的状态再分叉一个 agent）。
- **局限**：overlayfs 的 lower 必须是已经物化的真实目录，不认识 git；修改大文件会整体 copy-up；base 更新（比如 main 前进了，要 rebase）需要重建层。

### 2.4 块级 / 文件级 CoW 快照

| 方案 | 特点 |
|---|---|
| btrfs / ZFS 子卷快照 | 秒级快照整个工作区，块级共享，可 send/receive |
| reflink（XFS、btrfs、APFS 的 `cp --reflink` / `clonefile`） | 文件级 CoW 复制，复制一个工作区几乎不占空间 |
| LVM thin、云盘快照 | 块设备级 |
| CRIU、Firecracker microVM 快照 | 连进程内存一起 checkpoint，可用于"环境整体回滚" |

**要点：** 如果宿主文件系统支持 reflink，"worktree + reflink 复制依赖目录"就已经能拿到 overlay 的大部分好处，而且没有 FUSE 的性能代价。这是很强的基线，tingly-fuse 必须证明自己比它好。

### 2.5 面向 AI agent 的新方案（2025 年以来）

| 方案 | 做法 | 与本项目的关系 |
|---|---|---|
| **Dagger container-use** | 每个 agent 一个容器 + 一个 git 分支，工作通过 git 同步回来 | 解决了运行时隔离；工作区隔离仍依赖容器内的完整 checkout |
| **GitButler 并行分支** | 一个工作区同时"应用"多条分支，按 agent 会话把改动路由到对应分支提交 | 不建 worktree；但官方文档明确说**没有运行时/文件隔离**，两个 agent 改同一文件需要人为决定 |
| **Turso AgentFS** | 以原目录为只读底层，所有写入进 SQLite 差量库，删除用 whiteout，`agentfs mount` 挂载 | 最接近 tingly-fuse 的思路，但底层是普通目录而非 git 对象，也不管 git 元数据 |
| **Jujutsu（jj）workspaces** | 工作区本身就是一个自动更新的 commit，带操作日志可撤销 | "工作区即 commit"的模型很适合 agent checkpoint |
| **云沙箱**（E2B、Daytona、Modal 等） | microVM / 容器 + 快照 | 运行时隔离和快照，文件层仍是普通文件系统 |

**观察：** 现有 agent 方案要么只管运行时（容器），要么只管分支路由（GitButler），要么只管文件 CoW（AgentFS）。**"git 对象做底层 + 每 agent CoW 差量 + 每 agent 独立 git 元数据 + 跨 agent 冲突感知"这一组合目前没有现成产品**，这是 tingly-fuse 的空间。

---

## 3. 方案横向对比

评分：●●● 好　●● 中　● 差

| 维度 | 共享仓库切分支 | worktree | worktree + reflink | overlayfs（内核） | GitButler 并行分支 | 容器 + 分支（container-use） | **git 驱动 FUSE（tingly-fuse 目标）** |
|---|---|---|---|---|---|---|---|
| 工作区隔离 | ● | ●●● | ●●● | ●●● | ● | ●●● | ●●● |
| 创建速度 | ●●● | ● | ●● | ●● | ●●● | ● | ●●● |
| 磁盘占用 | ●●● | ● | ●●● | ●●● | ●●● | ● | ●●● |
| 依赖/构建缓存复用 | ●●● | ● | ●● | ●●● | ●●● | ●● | ●●● |
| 文件 IO 性能 | ●●● | ●●● | ●●● | ●●● | ●●● | ●●● | ●（需优化） |
| git 兼容性 | ●●● | ●●● | ●●● | ●●（元数据外置） | ●●（需工具配合） | ●●● | ●●（需自己做） |
| checkpoint / 回滚 | ● | ● | ●● | ●● | ●● | ●● | ●●● |
| 跨 agent 冲突提前发现 | ● | ● | ● | ●（能比较 upper） | ●● | ● | ●●● |
| 运行时隔离 | ● | ● | ● | ●（配合 namespace） | ● | ●●● | ●（需配合容器） |
| 实现复杂度（越高越简单） | ●●● | ●●● | ●●● | ●● | ●●● | ●● | ● |

读这张表的方式：FUSE 方案唯一明显的代价是 **IO 性能和实现复杂度**，唯一独有的收益是 **O(1) 创建 + 懒加载 + 细粒度 checkpoint + 冲突感知**。仓库越大、agent 越多、checkpoint 越频繁，FUSE 越划算。

---

## 4. 关键设计洞察

### 4.0 核心原则：共享存储，隔离工作空间，路径保持一致

NFS 这类方案提供的是"共享同一个存储"，而不是"共享同一个工作空间"：服务端可以按客户端或挂载点分别管理导出内容，但每个客户端看到的都是同一个挂载路径。tingly-fuse 应该采用同样的模型：**所有 agent 都在同一个路径（如 `/workspace/repo`）下工作，这个路径背后是每个 agent 自己的视图。**

和 worktree（每个 agent 一个不同目录）相比，路径一致带来的好处：

- 构建缓存、编译产物、venv shebang、IDE/LSP 索引中的绝对路径一致，可跨 agent 复用，无需重写路径；
- agent 的提示词、脚本、工具配置统一写同一个路径，agent 不需要知道自己在哪个工作区；
- fork、迁移、回滚时只是替换挂载背后的视图，进程看到的路径和环境不变。

实现方式：

| 方式 | 做法 | 评价 |
|---|---|---|
| 每 agent 独立挂载命名空间（推荐） | 每个 agent 的容器或 mount namespace 把自己的视图挂到同一路径；后端可以是用户态 NFS 服务按会话导出，也可以是 FUSE | 隔离干净，内核缓存按挂载区分，性能正常。NFS 后端的挂载由宿主完成，容器内无需 `/dev/fuse` 和特权 |
| 单挂载点按调用者区分 | 一个 FUSE 挂载，根据请求头中的 pid/uid/gid 或 cgroup 判断是哪个 agent，返回不同内容 | 内核页缓存和 dentry 缓存为所有调用者共享，必须关闭缓存，性能差；子进程、守护进程的归属容易判错。不推荐 |

**共享还是隔离，是 IO 层的路由策略，而不是二选一的架构。** 同一路径下，每次读写落到哪一层由策略决定，可以按会话、按目录配置：

| 模式 | IO 路由 | 适用场景 |
|---|---|---|
| 隔离 | 读：自己的 delta → 共享底层；写：自己的 delta | 互不相关的任务、同一任务多种尝试 |
| 共享 | 多个会话指向同一个可写层，彼此实时可见（类似 GitButler 的单工作区） | 紧密协作，如一个 agent 写代码、另一个同时跑测试或审阅 |
| 单向可见 | 把另一个会话的 delta 作为只读层叠在自己下面，写入仍进自己的 delta | 依赖另一个 agent 尚未提交的改动，又不想被它影响后续写入 |
| 按目录混合 | 例如 `src/` 隔离，`docs/`、`.cache/`、依赖目录共享 | 共享缓存和产物，同时隔离源码改动 |

这样，worktree（全隔离）和"共用一个 checkout 切分支"（全共享）只是这张策略表的两个极端，tingly-fuse 可以在运行时切换模式，例如先共享协作，再把某个会话的改动"分离"成独立分支。

因此后端协议（FUSE 或 NFS）是可替换的实现细节，对外契约是"**同一路径 + 按会话、按目录可配置的 IO 路由**"。Linux 容器场景可优先考虑 NFS 导出，避免容器内 FUSE 的权限问题；需要更细的控制（passthrough、精确的变更通知）时再用 FUSE。

### 4.1 底层用 git tree，而不是物化目录

overlayfs/AgentFS 的底层是"磁盘上的真实目录"，意味着每个不同的 base commit 都要物化一次。如果底层直接是 git tree：

- 任意 commit 都可以零成本作为 base（只是一个 OID）；
- 多个 agent 基于不同 commit 也共享同一个对象库；
- 未修改文件的内容就是 blob OID，`diff`/`status` 可以直接比较 OID，不用读内容。

### 4.2 每个 agent 一套"轻 gitdir"

参考 `git worktree` 的目录结构：每个 agent 有独立的 `HEAD`、`index`、`logs`，通过 `commondir` 指向共享对象库与 refs。工作区文件由 FUSE 提供。这样 agent 内部看到的就是一个正常仓库，能直接运行 `git status/add/commit`。

分支命名建议约定命名空间，例如 `agent/<session-id>/<task>`，避免冲突，也方便协调者批量清理。

### 4.3 文件系统负责回答"改了什么"

git 在大工作区上慢，主要慢在扫描。tingly-fuse 的差量层本身就是"被改动文件的精确集合"，可以：

- 实现 git 的 **fsmonitor hook（协议 v2）**，让 `git status` 只看差量层里的路径；
- 维护一个 journal（序号 + 路径），给 dev server、构建工具提供变更流（代替在 FUSE 上不可靠的 inotify）。

### 4.4 分层：代码层 / 依赖层 / agent 层

```
            agent A 视图          agent B 视图          agent C（从 A 分叉）
          ┌──────────────┐     ┌──────────────┐     ┌──────────────┐
  可写    │  delta A      │     │  delta B      │     │  delta C      │
          └──────┬───────┘     └──────┬───────┘     ├──────────────┤
                 │                    │             │  A@ckpt-3（冻结）│
          ┌──────┴────────────────────┴─────────────┴──────────────┐
  只读    │  依赖层：node_modules / .venv / target（按 lockfile 哈希缓存）│
          ├─────────────────────────────────────────────────────────┤
  只读    │  代码层：git tree @ base commit（按需从对象库加载）          │
          └─────────────────────────────────────────────────────────┘
                              共享对象库（bare repo / partial clone）
```

- 依赖层以 lockfile 内容哈希为键，跨 agent、跨 base 复用，相当于 Docker 的 "RUN npm ci" 层缓存；
- agent 想升级依赖时，写入落在自己的 delta，不影响别人；
- **fork**：把某 agent 的 delta 冻结为只读层，在上面叠新 delta，就得到一个分叉 agent（适合"同一任务多种尝试"）。

### 4.5 Checkpoint 有两种粒度

| 类型 | 实现 | 用途 |
|---|---|---|
| 文件系统 checkpoint | 冻结当前 delta 为只读层，新开一层 | 高频、廉价，agent 每一步工具调用后都可以打，用于回滚 |
| git checkpoint | 把"base tree + delta"写成 tree 对象，生成 commit（可放在隐藏 ref 如 `refs/tingly/<session>/ckpt/N`） | 可持久化、可推送、可被别的 agent 作为 base |

第二种让"checkpoint"和"git 历史"统一：任何快照都能直接变成分支或 PR（这也是 jj、Sapling 的思路）。

### 4.6 冲突感知：写时就知道

因为所有 agent 的 delta 都在同一个守护进程里，它可以实时知道"A 和 B 都改了 `src/foo.ts`"。可以做：

- 路径级重叠告警（推送给协调者）；
- 可选的软锁 / 意图声明（agent 先声明要改的目录）；
- 合并前预演：在内存里对两个 delta 做三方合并，提前报冲突。

这是 worktree、容器方案都无法提供的能力，也是"多 agent 协同"真正的价值所在。

### 4.7 Base 前进时的处理（rebase）

main 前进后，agent 的 base 需要更新。由于 delta 只记录改动文件：

- 未改动的文件直接换成新 base 的 blob，零成本；
- 改动过的文件做三方合并（旧 base、新 base、delta），冲突的标记出来交给 agent。

相比之下 overlayfs 要重新物化 lower，worktree 要真的 `git rebase`。

---

## 5. 推荐路线

### 阶段 0：零 FUSE 基线（先测出天花板）

- 一个共享 bare repo（可 partial clone）；
- 每 agent：`git worktree add` 或基于 `GIT_DIR/GIT_INDEX_FILE` 的轻工作区；
- 依赖目录用 `cp --reflink` 或 overlayfs（user namespace 内）挂载共享的依赖层；
- 统一的 session 管理 CLI：create / list / checkpoint（btrfs 快照或 reflink 复制）/ commit / destroy。

产出：一组基准数据（创建时间、磁盘、`npm install`/build/test 耗时、`git status` 耗时）。**如果这一档已经够用，FUSE 的优先级就可以下调。**

### 阶段 1：tingly-fuse MVP（Linux only）

- FUSE 守护进程，底层 = git tree（libgit2/gitoxide 读对象），上层 = 每 session 一个差量目录；
- 每 session 一个轻 gitdir，实现 fsmonitor hook；
- 文件系统 checkpoint（冻结层）+ 回滚；
- 依赖层仍可先用 overlayfs/reflink，不急着全进 FUSE。

### 阶段 2：协同能力

- git checkpoint（delta → commit）、fork、base rebase；
- 跨 session 冲突告警与合并预演；
- 给 agent 和协调者暴露 API（CLI + MCP 工具）：`session.create(base)`, `session.fork(from, ckpt)`, `session.checkpoint()`, `session.rollback(n)`, `session.commit(branch)`, `session.diff(other)`。

### 推荐的整体架构

```
 ┌───────────── 协调者 / agent（通过 CLI 或 MCP 调用）─────────────┐
 │                                                                │
 │   Session Manager ── 生命周期、命名空间、配额、GC               │
 │        │                                                       │
 │   Conflict Monitor ── 路径重叠、三方合并预演、告警              │
 │        │                                                       │
 │   FUSE Daemon ──┬── Tree Resolver（path → tree/blob OID 缓存）  │
 │                 ├── Layer Stack（依赖层 / 冻结层 / 可写 delta）  │
 │                 ├── Journal（变更流，供 fsmonitor 和 watcher）   │
 │                 └── Git Shim（每 session 的 HEAD/index/refs）    │
 │                                                                │
 │   Shared Object Store（bare repo，partial clone，后台 gc/repack）│
 └────────────────────────────────────────────────────────────────┘
```

---

## 6. 风险与难点

| 风险 | 说明 | 缓解思路 |
|---|---|---|
| FUSE 元数据性能 | `npm install`、大量小文件、频繁 stat 会放大用户态往返 | 开启内核 entry/attr 缓存、readdirplus、writeback cache；Linux 6.9+ 的 FUSE passthrough 让 delta 中的文件读写直通底层；依赖层放在 overlayfs 而不是 FUSE |
| 文件监听 | inotify 对 FUSE 支持有限，dev server 热更新可能失效 | 守护进程主动提供变更流；或让构建工具用 watchman 接口 |
| git index 一致性 | index 中的 stat 信息与 FUSE 报告的 mtime/inode 必须稳定 | 为未改动文件返回确定性的 inode/mtime；fsmonitor 兜底 |
| 容器内挂载 | 需要 `/dev/fuse` 和相应权限 | 守护进程跑在宿主，以 bind mount 方式给容器；或使用支持 FUSE 的沙箱运行时 |
| 大文件 copy-up | 改一个字节也复制整个文件 | 首版接受；后续可做块级差量 |
| 运行时隔离 | 端口、数据库、环境变量不归文件系统管 | 明确边界：tingly-fuse 管文件与 git，运行时交给容器/namespace |
| 跨平台 | macOS FUSE 依赖内核扩展或 FUSE-T，Windows 要 ProjFS | 首版只做 Linux；macOS 可参考 EdenFS 改走本地 NFS |

---

## 7. 需要你拍板的问题

1. **agent 运行在哪里？** 都在 Linux 容器里，还是也要支持开发者本机 macOS？这决定是否只做 Linux FUSE。（建议：先只做 Linux）
2. **典型仓库规模？** 文件数和体积决定懒加载的收益有多大，也决定阶段 0 是否已经够用。
3. **是否需要先做阶段 0 基线？**（建议：做，用数据决定 FUSE 投入）
4. **冲突策略偏好？** 只告警、软锁、还是协调者自动合并？
5. **实现语言？** Rust（gitoxide + fuser，性能和内存安全）或 Go（go-fuse + go-git，开发快）。（建议：Rust）

---

## 参考资料

- Turso AgentFS overlay 文档：https://docs.turso.tech/agentfs/guides/overlay
- Turso AgentFS 博客：https://turso.tech/blog/agentfs-overlay
- AgentFS 源码：https://github.com/tursodatabase/agentfs
- GitButler 并行 agent 文档：https://docs.gitbutler.com/ai-agents/parallel-agents
- Trigger.dev，用 GitButler 替代 worktree 的实践：https://trigger.dev/blog/parallel-agents-gitbutler
- Dagger container-use：https://pkg.go.dev/github.com/dagger/container-use
- InfoQ 对 container-use 的介绍：https://www.infoq.com/news/2025/08/container-use
- Sapling / EdenFS：https://github.com/facebook/sapling
- VFS for Git：https://github.com/microsoft/VFSForGit
- Scalar（git 文档）：https://git-scm.com/docs/scalar
- Google 单仓与 CitC：Potvin & Levenberg, "Why Google Stores Billions of Lines of Code in a Single Repository", CACM 2016
- Linux overlayfs 文档：https://docs.kernel.org/filesystems/overlayfs.html
- git fsmonitor 钩子：https://git-scm.com/docs/githooks#_fsmonitor_watchman
