# 0013 命令分两层：通用层和 `git` 子命令

- 状态：已采纳
- 日期：2026-10-21

## 背景

到 PR #9 为止，每个仓库都必须有 upstream，每个会话都从一个 commit 出发、有自己的分支，共享路径里也夹着 Git 的判断（被跟踪的路径、`info/exclude`、`.git`）。YZ 的问题是：命令里对 Git 特殊处理这么多，不如直接开一个新命令来服务带 Git 的情况。方案贴在 PR #9 的讨论里，YZ 选了先合并 #9、再单开一个 PR 拆分。看了第一版后，YZ 指出 `git add`、`git spawn`、`git check` 这些名字和 git 自己的命令对不上，用的人要多一层理解，于是子命令改成按 git 的命令命名。

## 决定

1. **两种仓库。** `repo.json` 新增 `kind`：
   - `dir`：任意目录。`base add <name> <dir>` 第一次登记它，每次运行都把目录 reflink 快照成一个新 base（`snap-<时间>-<随机>`），记为 `current`，新会话从它出发；已有会话不受影响。
   - `git`：`git clone <upstream> [<name>]` 登记，镜像、按 commit 建 base、固定 ref、共享 `.git` 都和以前一样。
   - 旧版本写的 `repo.json` 没有 `kind`，按 `git` 读，不需要迁移。
2. **通用命令两种仓库都能用：** `exec`、`drop`、`list`、`share`、`unshare`、`base list [name]`、`base rm`、`base prune`、`remove`。`spawn` 只用于 `dir`；`git` 仓库用 `git worktree add`。两边遇到对方的仓库都报错，并说该用哪个命令。
3. **`git` 子命令按 git 的命令命名，参数顺序也跟 git 一样。** 会话对应 git 的 worktree：从同一个仓库出发，有自己的工作区、HEAD 和分支。

   | tingly-fuse | 对应的 git | 原来的命令 |
   |---|---|---|
   | `git clone <upstream> [<name>] [--shared-git] [--share p]` | `git clone <repo> [<dir>]`，名字默认取 URL 最后一段去掉 `.git` | `repo add <name> <upstream>` |
   | `git worktree add <name> [<commit-ish>] [-b <branch>]`，另有 `--from-dir`、`--base`、`--no-fetch` | `git worktree add [-b <branch>] <path> [<commit-ish>]` | `spawn <repo> --from <rev> --branch <b>` |
   | `git worktree list [<name>]` | `git worktree list` | `list` 里的 git 会话 |
   | `git worktree remove <session>` | `git worktree remove` | `drop` |
   | `git push <session> [<remote>]` | `git push [<remote>]` | `push <session> --remote <r>` |
   | `git fetch <name>` | `git fetch` | `repo fetch` |
   | `git fsck <name> [--fix]` | `git fsck`；`--fix` 给旧 base 补固定 ref | `repo check` |

   选项可以放在位置参数前后，和 git 一样。`git add`、`git spawn`、`git check` 报错并给出新名字。
4. **通用层不解释目录内容。** 不 fetch、不建分支、不判断哪些路径被跟踪、不写 `info/exclude`。目录里如果有 `.git`，它也只是普通文件，每个会话一份私有拷贝。唯一保留的是 `.git` 不能设为共享路径：共享它就等于所有会话共用 HEAD 和 index。
5. **`current` 不会被删。** `base rm` 拒绝删除它，`base prune` 跳过它，先 `base add` 一个新快照才能删旧的。
6. **旧命令明确报错，不保留别名。** `repo ...` 和 `push` 会报错并列出新名字。
7. **Go API 也分两层。** 通用：`Manager.AddDir`、`Manager.Spawn(SpawnOptions)`，以及 `Exec`、`Drop`、`Sessions`、`Shares`、`SetShares`、`Bases`、`RemoveBase`、`PruneBases`、`RemoveRepo`、`Repos`、`Repo`。Git：`Manager.Git()` 返回 `*Git`，方法同样按 git 命名：`Clone`、`AddWorktree(WorktreeOptions)`、`Worktrees`、`RemoveWorktree`、`Push`、`Fetch`、`Fsck`。原来的 `AddRepo`、`AddRepoWith`、`FetchRepo`、`CheckRepo`、`Push` 去掉。

## 理由

- 视图、共享路径、base 回收本来就和 Git 无关，拆开以后通用层的代码里不再有 Git 分支，Git 的逻辑集中在 `internal/manager/git.go` 和 `internal/repo` 的 git 部分。
- 没有 Git 的目录（数据目录、还没 init 的项目）也能用；有 Git 但不想要镜像和分支的目录，也可以当普通目录用。
- 两种仓库共用 `repos/<name>/`、`bases/`、`shares/` 和会话存储，base 回收、共享路径、后端都只有一份实现。

## 被放弃的选项

- **自己起名的 git 子命令（`git add`、`git spawn`、`git check`）。** 第一版就是这样，`git add` 在 git 里是暂存文件，和"登记仓库"意思冲突，用的人要记两套词。
- **`spawn` 自动识别仓库类型。** 等于把 Git 判断又放回通用命令里，而且 `--from`、`--branch` 这些参数对 `dir` 仓库没有意义。
- **保留 `repo ...` 作为别名。** 两套名字长期并存，文档和报错都要讲两遍；不如一次改完，旧名字报错时直接告诉新名字。
- **`dir` 仓库单独放一个目录（如 `dirs/<name>`）。** 名字冲突检查、base 回收、共享路径都要写两份。

## 后果

- 命令和 Go API 都是破坏性变更。[0007](0007-from-dir-snapshot-and-absorb-objects.md)、[0010](0010-shared-git-via-registered-worktrees.md)、[0011](0011-shared-paths.md)、[0012](0012-public-go-api.md) 里的旧命令名以这里为准：`repo add` 现在是 `git clone`，`spawn --from-dir` 现在是 `git worktree add --from-dir`，`repo share` 现在是 `share`。
- `dir` 仓库的共享路径不会写进 `info/exclude`。目录里有 `.git` 时，共享路径会出现在 `git status` 里，要排除就自己写 `.gitignore`，或者改用 `git` 仓库。
- `dir` 会话没有分支，`Session.Branch` 为空，`list` 显示 `-`。

## 验证（2026-10-21，Linux，copy 和 overlay，root 与 uid 65534）

- `internal/manager/dir_test.go`：快照之后改源目录，已有会话看不到；第二次 `base add` 只影响之后的会话；`--base` 能从旧快照出发；共享路径互相可见、其余文件私有；当前快照不会被 prune 或 rm（去掉这项保护时测试失败）；目录里的 `.git` 每会话一份，提交不互相影响，被跟踪的文件也能共享（让通用层去判断被跟踪路径时测试失败）；两层互相拒绝对方的仓库（去掉 `spawn` 对 git 仓库的拒绝时测试失败）；登记失败不留目录。
- CLI 测试覆盖全部新命令、旧命令的报错，以及 `base list` 的两种输出；`TestAPI` 只用公开包走完两层。
- `TestRepoNameFromURL`：`git clone` 不给名字时，https、`.git` 后缀、结尾斜杠、scp 风格地址、本地路径都取到和 git 一样的名字。CLI 测试覆盖选项放在位置参数前后、`git worktree list/remove` 只认 git 会话。
- `bench/lifecycle.sh` 在真实 tingly-box 上用 `git` 子命令跑完，copy 和 overlay 都 0 失败；用 PR #9 合并后的旧版本建 base、新版本检查和修复（升级路径），也是 0 失败。
- 在 tingly-box 的浅克隆（64 MB）上：`base add` 1.3 秒，两个 overlay 会话各占约 400 KB，共享的 `.cache` 互相可见，一个会话改的 `README.md` 另一个看不到。
