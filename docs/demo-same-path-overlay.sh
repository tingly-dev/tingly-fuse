#!/usr/bin/env bash
# 演示：两个 agent 都在 /workspace/tingly-box 下工作，但各自读写不同的 overlay。
# 需要 Linux（root 或支持 unprivileged user namespace 的内核 5.11+）。
set -euo pipefail

SRC=${1:?usage: $0 <path-to-git-checkout>}
ROOT=${ROOT:-/tmp/tingly-demo}
rm -rf "$ROOT"; mkdir -p "$ROOT"

# 1. 共享只读底层：一份带 .git 的 checkout，所有 agent 共用
cp -a "$SRC" "$ROOT/base"
# 对象库移出 overlay，改用 alternates 指向共享存储。
# 否则 git 会刷新 pack 文件的 mtime（freshen），触发 overlay 把整个 pack 复制到差量层。
mv "$ROOT/base/.git/objects" "$ROOT/objects"
mkdir -p "$ROOT/base/.git/objects/info" "$ROOT/base/.git/objects/pack"
echo "$ROOT/objects" > "$ROOT/base/.git/objects/info/alternates"
# 模拟远端
git clone -q --bare "$ROOT/base" "$ROOT/remote.git"

# 2. 在 agent 自己的 mount namespace 里把它的视图挂到同一路径
run_agent() {
  local name=$1; shift
  mkdir -p "$ROOT/$name/upper" "$ROOT/$name/work"
  unshare -m --propagation private bash -c '
    set -e
    mkdir -p /workspace/tingly-box
    mount -t overlay overlay \
      -o lowerdir='"$ROOT"'/base,upperdir='"$ROOT/$name"'/upper,workdir='"$ROOT/$name"'/work \
      /workspace/tingly-box
    cd /workspace/tingly-box
    '"$*"'
  '
}

GIT='git -c user.name=demo -c user.email=demo@example.com'

echo "== agent-a：改 README，提交到自己的分支"
run_agent agent-a "
  $GIT switch -q -c agent-a/task
  echo 'edited by agent-a' >> README.md
  $GIT commit -qam 'agent-a change'
  echo \"pwd=\$PWD  branch=\$(git branch --show-current)\"
  tail -1 README.md
  $GIT push -q $ROOT/remote.git agent-a/task
"

echo "== agent-b：同一路径，看不到 agent-a 的改动"
run_agent agent-b "
  echo \"pwd=\$PWD  branch=\$(git branch --show-current)\"
  tail -1 README.md
  $GIT switch -q -c agent-b/task
  echo 'hello from agent-b' > NOTES.md
  $GIT add NOTES.md && $GIT commit -qm 'agent-b change'
  $GIT push -q $ROOT/remote.git agent-b/task
"

echo "== 宿主视角"
echo "base 仍是原样：$(tail -1 $ROOT/base/README.md)"
echo "agent-a 的差量层大小：$(du -sh $ROOT/agent-a/upper | cut -f1)，agent-b：$(du -sh $ROOT/agent-b/upper | cut -f1)，底层：$(du -sh $ROOT/base | cut -f1)"
echo "远端分支：$(git --git-dir=$ROOT/remote.git branch --format='%(refname:short)' | tr '\n' ' ')"
