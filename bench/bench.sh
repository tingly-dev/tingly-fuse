#!/usr/bin/env bash
# 对比多 agent 工作区方案：worktree / copy / reflink / overlay。
#
# 每种方案依次建 N 个会话，记录创建耗时、磁盘增量，以及在会话里跑
# git status、pnpm install、go build 的耗时。磁盘用专用文件系统的
# statfs 已用量增量计算，硬链接、reflink 共享块、overlay 差量层都能如实反映。
#
# 用法：
#   bench.sh setup     准备镜像、子模块镜像、pnpm store、Go 缓存（需联网，只跑一次）
#   bench.sh run       跑全部方案，结果写到 $OUT/results.csv
#
# 主要环境变量：
#   BENCH_FS        专用文件系统挂载点（默认 /mnt/xfs，需支持 reflink 才能测 reflink）
#   BENCH_METHODS   要跑的方案（默认 "worktree copy reflink overlay"）
#   BENCH_N         每种方案的会话数（默认 3）
#   BENCH_REV       基础 commit（默认镜像 HEAD）
#   BENCH_WARM_BASE 设为 1 时，基础 checkout 预装 node_modules（测"预热底层"场景）
#
# copy 和 overlay 走 tingly-fuse 自己的后端；worktree 和 reflink 由脚本直接实现。
# overlay 需要 Linux（root 或 unprivileged user ns），视图挂在 $TINGLY_FUSE_VIEW_ROOT（默认 /workspace）下。
set -euo pipefail

UPSTREAM=${BENCH_UPSTREAM:-https://github.com/tingly-dev/tingly-box}
FS=${BENCH_FS:-/mnt/xfs}
ROOT=$FS/bench
CACHE=$FS/bench-cache                 # 镜像和 pnpm store 放在同一文件系统，才能硬链接/reflink
MIRROR=$CACHE/tingly-box.git
SUBS=$CACHE/sub                       # 子模块镜像：<name>.git
STORE=$CACHE/pnpm-store
METHODS=${BENCH_METHODS:-worktree copy reflink overlay}
N=${BENCH_N:-3}
WARM=${BENCH_WARM_BASE:-0}
OUT=${BENCH_OUT:-$PWD/bench-out}
TF=${TINGLY_FUSE:-tingly-fuse}

export CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0
# 子模块从本地镜像取，不走网络；对所有 git 调用生效，不改任何全局配置。
export GIT_CONFIG_COUNT=3
export GIT_CONFIG_KEY_0=url.$SUBS/.insteadOf GIT_CONFIG_VALUE_0=https://github.com/tingly-dev/
export GIT_CONFIG_KEY_1=protocol.file.allow GIT_CONFIG_VALUE_1=always
export GIT_CONFIG_KEY_2=advice.detachedHead GIT_CONFIG_VALUE_2=false
export GIT_AUTHOR_NAME=bench GIT_AUTHOR_EMAIL=bench@example.com
export GIT_COMMITTER_NAME=bench GIT_COMMITTER_EMAIL=bench@example.com

log() { echo "[$(date +%T)] $*" >&2; }
now() { date +%s.%N; }
used() { sync -f "$FS"; df --output=used -B1 "$FS" | tail -1; }
mb() { awk -v b="$1" 'BEGIN{printf "%.1f", b/1048576}'; }

# ---------- setup ----------

setup() {
  mkdir -p "$CACHE" "$SUBS"
  [ -d "$MIRROR" ] || GIT_CONFIG_COUNT=0 git clone -q --mirror "$UPSTREAM" "$MIRROR"
  local tmp=$CACHE/warm
  rm -rf "$tmp"
  git clone -q "$MIRROR" "$tmp"
  git -C "$tmp" config -f .gitmodules --get-regexp '\.url$' | while read -r _ url; do
    local name; name=$(basename "$url")
    [ -d "$SUBS/$name" ] || GIT_CONFIG_COUNT=0 git clone -q --mirror "$url" "$SUBS/$name"
  done
  git -C "$tmp" submodule update --init -q
  # 预热 pnpm store 和 Go 模块/构建缓存，后面所有测量都是"缓存已热"的情形。
  (cd "$tmp/frontend" && pnpm install --frozen-lockfile --store-dir "$STORE" >/dev/null)
  (cd "$tmp" && go build -o /dev/null ./cli/tingly-box)
  rm -rf "$tmp"
  log "setup done"
}

# ---------- 基础 checkout（copy / reflink / overlay 共用） ----------

TFHOME=$ROOT/tf
tf() { TINGLY_FUSE_HOME=$TFHOME "$TF" "$@"; }

# 让 tingly-fuse 建出基础 checkout，再补上子模块（tingly-fuse 目前不处理子模块）。
# 子模块对象同样用 alternates 指向镜像，基础目录里不留对象。
prepare_base() {
  rm -rf "$TFHOME"
  tf git clone "$MIRROR" tingly-box >/dev/null
  tf git worktree add tingly-box "$REV" --backend copy --id prep >/dev/null
  tf drop prep
  BASE=$TFHOME/repos/tingly-box/bases/$REV
  git -C "$BASE" submodule update --init -q
  git -C "$BASE" submodule foreach -q '
    m=$(git rev-parse --git-dir)
    echo "'"$SUBS"'/$(basename "$(git config remote.origin.url)")/objects" > "$m/objects/info/alternates"
    git repack -adlq && git prune'
  if [ "$WARM" = 1 ]; then
    (cd "$BASE/frontend" && pnpm install --frozen-lockfile --offline --store-dir "$STORE" >/dev/null)
  fi
  git -C "$BASE" status --porcelain >/dev/null   # 刷新基础 index 的 stat 信息
  log "base ready: $BASE ($(du -sh --exclude=.git "$BASE" | cut -f1) tree)"
}

# ---------- 各方案：create / inws / destroy / name ----------

# worktree：一个主 clone，会话是 git worktree，子模块每个 worktree 单独 clone。
wt_prepare() {
  rm -rf "$ROOT/wt"; mkdir -p "$ROOT/wt"
  git clone -q "$MIRROR" "$ROOT/wt/main"
  git -C "$ROOT/wt/main" checkout -q --detach "$REV"
  git -C "$ROOT/wt/main" submodule update --init -q
  if [ "$WARM" = 1 ]; then
    (cd "$ROOT/wt/main/frontend" && pnpm install --frozen-lockfile --offline --store-dir "$STORE" >/dev/null)
  fi
}
wt_create() {
  git -C "$ROOT/wt/main" worktree add -q -b "agent/$1" "$ROOT/wt/$1" "$REV"
  git -C "$ROOT/wt/$1" submodule update --init -q
}
wt_inws() { (cd "$ROOT/wt/$1" && bash -c "$2"); }
wt_cleanup() { rm -rf "$ROOT/wt"; }

# copy：tingly-fuse 自带的 copy 后端。
copy_prepare() { prepare_base; }
copy_create() { tf git worktree add tingly-box "$REV" --backend copy --id "$1" >/dev/null; }
copy_inws() { tf exec "$1" -- bash -c "$2"; }
copy_cleanup() { rm -rf "$TFHOME"; }

# reflink：和 copy 一样整份复制基础 checkout，但用 cp --reflink=always（Linux 上对应 APFS clonefile）。
rl_prepare() { prepare_base; rm -rf "$ROOT/rl"; mkdir -p "$ROOT/rl"; }
rl_create() {
  cp -a --reflink=always "$BASE" "$ROOT/rl/$1"
  git -C "$ROOT/rl/$1" switch -q -c "agent/$1"
}
rl_inws() { (cd "$ROOT/rl/$1" && bash -c "$2"); }
rl_cleanup() { rm -rf "$ROOT/rl" "$TFHOME"; }

# overlay：tingly-fuse 的 overlay 后端（mount namespace + overlayfs，所有会话同一路径）。
ov_prepare() { prepare_base; }
ov_create() { tf git worktree add tingly-box "$REV" --backend overlay --id "$1" >/dev/null; }
ov_inws() { tf exec "$1" -- bash -c "$2"; }
ov_cleanup() { rm -rf "$TFHOME"; }

prefix() { case $1 in worktree) echo wt;; copy) echo copy;; reflink) echo rl;; overlay) echo ov;; *) echo "unknown method $1" >&2; exit 2;; esac; }

# ---------- 测量 ----------

record() {  # method session step seconds disk_delta_bytes
  printf '%s,%s,%s,%s,%.3f,%s\n' "$WARMTAG" "$1" "$2" "$3" "$4" "$(mb "$5")" | tee -a "$CSV"
}

# measure <method> <session> <step> <command...>
measure() {
  local m=$1 s=$2 step=$3; shift 3
  local u0 t0 t1 u1
  u0=$(used); t0=$(now)
  "$@" >>"$OUT/log.txt" 2>&1 || { log "FAILED: $m $s $step"; tail -20 "$OUT/log.txt" >&2; return 1; }
  t1=$(now); u1=$(used)
  record "$m" "$s" "$step" "$(awk -v a="$t0" -v b="$t1" 'BEGIN{print b-a}')" $((u1 - u0))
}

PNPM="cd frontend && pnpm install --frozen-lockfile --offline --store-dir $STORE"
GOBUILD="go build -o /dev/null ./cli/tingly-box"
GITST="git status --porcelain >/dev/null"

run_method() {
  local m=$1 p; p=$(prefix "$m")
  log "== $m: prepare"
  "${p}_prepare"
  local base_used; base_used=$(used)
  for i in $(seq 1 "$N"); do
    measure "$m" "s$i" create "${p}_create" "s$i"
  done
  # 会话 1 和 2 上跑完整流程：首次 git status（冷 index）、再次 git status、pnpm、go build、改动后 git status
  for i in 1 2; do
    [ "$i" -le "$N" ] || break
    measure "$m" "s$i" git-status-1 "${p}_inws" "s$i" "$GITST"
    measure "$m" "s$i" git-status-2 "${p}_inws" "s$i" "$GITST"
    measure "$m" "s$i" pnpm-install "${p}_inws" "s$i" "$PNPM"
    measure "$m" "s$i" go-build "${p}_inws" "s$i" "$GOBUILD"
    measure "$m" "s$i" git-status-3 "${p}_inws" "s$i" "$GITST"
  done
  record "$m" all total-disk 0 $(( $(used) - base_used ))
  "${p}_cleanup"
}

main() {
  case ${1:-} in
    setup) setup ;;
    run)
      mkdir -p "$OUT" "$ROOT"
      CSV=$OUT/results.csv
      [ -f "$CSV" ] || echo "base,method,session,step,seconds,disk_mb" >"$CSV"
      WARMTAG=$([ "$WARM" = 1 ] && echo warm-base || echo clean-base)
      REV=${BENCH_REV:-$(git -C "$MIRROR" rev-parse HEAD)}
      {
        echo "date: $(date -u +%FT%TZ)  rev: $REV  base: $WARMTAG  N=$N"
        echo "kernel: $(uname -r)  cpus: $(nproc)  fs: $(findmnt -no FSTYPE "$FS") at $FS"
        echo "git: $(git --version)  go: $(git -C "$MIRROR" show "$REV":go.mod | grep -m1 "^go ")（GOTOOLCHAIN=auto）  pnpm: $(git -C "$MIRROR" show "$REV":frontend/package.json | grep -o "pnpm@[0-9.]*")  tingly-fuse: $(command -v "$TF") @ $(git -C "$(dirname "$0")" rev-parse --short HEAD 2>/dev/null)"
      } | tee -a "$OUT/env.txt"
      for m in $METHODS; do run_method "$m"; done
      ;;
    *) sed -n '2,22p' "$0"; exit 2 ;;
  esac
}

main "$@"
