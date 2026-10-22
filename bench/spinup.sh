#!/usr/bin/env bash
# 从零到"能构建"：给 N 个 agent 各开一个工作区，装依赖、构建，对比几种做法的总耗时和磁盘。
#
# 和 bench.sh 的区别：这里把一次性成本（clone、建 base、准备目录）也算进去，
# 每种做法都从空目录开始，回答"开 N 个工作区并构建起来，总共要多久、占多少盘"。
#
# 做法：
#   clone      每个 agent 从上游完整 git clone --recurse-submodules（走网络）
#   worktree   上游 clone 一次；每个 agent git worktree add + git submodule update --init
#   tf         tingly-fuse：git clone 一次；每个 agent git worktree add（overlay 后端）
#   tf-share   同 tf，但 frontend/node_modules 用 --share 在所有会话间共用一份
#   tf-fromdir tingly-fuse：先在一个普通 checkout 里装好依赖，git worktree add --from-dir 做 base
#
# 每个工作区依次：创建、git status、pnpm install、go build -trimpath（共用 GOCACHE）。
# Go 构建缓存在每种做法开始前清空，所以第一个工作区是冷构建；模块缓存和 pnpm store 预热好，不走网络。
#
# 用法：bench/spinup.sh setup; bench/spinup.sh run
# 环境变量：BENCH_FS（默认 /mnt/bench，专用文件系统，磁盘用 statfs 增量）、BENCH_N（默认 3）、
#           BENCH_METHODS、BENCH_UPSTREAM、BENCH_TRIMPATH（默认 1；0 时不加 -trimpath）
set -euo pipefail

UPSTREAM=${BENCH_UPSTREAM:-https://github.com/tingly-dev/tingly-box}
FS=${BENCH_FS:-/mnt/bench}
ROOT=$FS/spinup
STORE=$FS/pnpm-store                     # 与工作区同一文件系统，pnpm 才能硬链接
CACHE=${BENCH_CACHE:-$HOME/.cache/tf-spinup}  # Go 缓存放在被测文件系统之外，不计入磁盘
METHODS=${BENCH_METHODS:-clone worktree tf tf-share tf-fromdir}
N=${BENCH_N:-3}
OUT=${BENCH_OUT:-$PWD/bench-out}
TF=${TINGLY_FUSE:-tingly-fuse}
TRIM=$([ "${BENCH_TRIMPATH:-1}" = 1 ] && echo -trimpath || true)

export CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0 GOTOOLCHAIN=auto
export GOMODCACHE=$CACHE/gomod GOCACHE=$CACHE/gocache
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=advice.detachedHead GIT_CONFIG_VALUE_0=false
export GIT_AUTHOR_NAME=bench GIT_AUTHOR_EMAIL=bench@example.com
export GIT_COMMITTER_NAME=bench GIT_COMMITTER_EMAIL=bench@example.com

log() { echo "[$(date +%T)] $*" >&2; }
now() { date +%s.%N; }
used() { sync -f "$FS"; df --output=used -B1 "$FS" | tail -1; }
mb() { awk -v b="$1" 'BEGIN{printf "%.1f", b/1048576}'; }

setup() {
  mkdir -p "$CACHE" "$STORE"
  local tmp=$FS/warm
  rm -rf "$tmp"
  git clone -q --recurse-submodules "$UPSTREAM" "$tmp"
  (cd "$tmp/frontend" && pnpm install --frozen-lockfile --store-dir "$STORE" >/dev/null)
  (cd "$tmp" && go mod download)
  rm -rf "$tmp"
  log "setup done"
}

TFHOME=$ROOT/tf
tf() { TINGLY_FUSE_HOME=$TFHOME "$TF" "$@"; }
in_dir() { (cd "$1" && bash -c "$2"); }

# 每种做法：<p>_once（一次性准备）、<p>_create <id>、<p>_in <id> <cmd>
clone_once() { :; }
clone_create() { git clone -q --recurse-submodules "$UPSTREAM" "$ROOT/$1"; }
clone_in() { in_dir "$ROOT/$1" "$2"; }

worktree_once() { git clone -q --recurse-submodules "$UPSTREAM" "$ROOT/main"; }
worktree_create() {
  git -C "$ROOT/main" worktree add -q -b "agent/$1" "$ROOT/$1" HEAD
  git -C "$ROOT/$1" submodule update --init -q
}
worktree_in() { in_dir "$ROOT/$1" "$2"; }

tf_once() { tf git clone "$UPSTREAM" tingly-box >/dev/null; }
tf_create() { tf git worktree add tingly-box --id "$1" >/dev/null; }
tf_in() { tf exec "$1" -- bash -c "$2"; }

tf-share_once() { tf git clone --share frontend/node_modules "$UPSTREAM" tingly-box >/dev/null; }
tf-share_create() { tf_create "$@"; }
tf-share_in() { tf_in "$@"; }

tf-fromdir_once() {
  git clone -q --recurse-submodules "$UPSTREAM" "$ROOT/prep"
  in_dir "$ROOT/prep" "$PNPM"
  tf git clone "$UPSTREAM" tingly-box >/dev/null
  tf git worktree add tingly-box --from-dir "$ROOT/prep" --id first >/dev/null
  tf drop first
  rm -rf "$ROOT/prep"   # base 是快照，准备目录用完即可删
}
tf-fromdir_create() { tf git worktree add tingly-box --base "$(ls "$TFHOME/repos/tingly-box/bases" | grep -m1 '^dir-')" --id "$1" >/dev/null; }
tf-fromdir_in() { tf_in "$@"; }

record() {  # method agent step seconds disk_bytes
  printf '%s,%s,%s,%.3f,%s\n' "$1" "$2" "$3" "$4" "$(mb "$5")" | tee -a "$CSV"
}

measure() {  # method agent step command...
  local m=$1 a=$2 step=$3; shift 3
  local u0 t0 t1 u1
  u0=$(used); t0=$(now)
  "$@" >>"$OUT/spinup-log.txt" 2>&1 || { log "FAILED: $m $a $step"; tail -20 "$OUT/spinup-log.txt" >&2; return 1; }
  t1=$(now); u1=$(used)
  record "$m" "$a" "$step" "$(awk -v a="$t0" -v b="$t1" 'BEGIN{print b-a}')" $((u1 - u0))
}

PNPM="cd frontend && pnpm install --frozen-lockfile --offline --store-dir $STORE"
GOBUILD="go build $TRIM -o /dev/null ./cli/tingly-box"
GITST="git status --porcelain >/dev/null"

run_method() {
  local m=$1
  rm -rf "$ROOT" "$GOCACHE"; mkdir -p "$ROOT"
  log "== $m"
  local u0 t0; u0=$(used); t0=$(now)
  measure "$m" setup once "${m}_once"
  for i in $(seq 1 "$N"); do
    measure "$m" "a$i" create "${m}_create" "a$i"
    measure "$m" "a$i" git-status "${m}_in" "a$i" "$GITST"
    measure "$m" "a$i" pnpm-install "${m}_in" "a$i" "$PNPM"
    measure "$m" "a$i" go-build "${m}_in" "a$i" "$GOBUILD"
  done
  record "$m" all total "$(awk -v a="$t0" -v b="$(now)" 'BEGIN{print b-a}')" $(( $(used) - u0 ))
  if [ -d "$TFHOME/sessions" ]; then
    for s in "$TFHOME"/sessions/*/; do tf drop "$(basename "$s")" >/dev/null 2>&1 || true; done
  fi
  rm -rf "$ROOT"
}

case ${1:-} in
  setup) setup ;;
  run)
    mkdir -p "$OUT"
    CSV=$OUT/spinup.csv
    [ -f "$CSV" ] || echo "method,agent,step,seconds,disk_mb" >"$CSV"
    echo "date: $(date -u +%FT%TZ)  upstream: $UPSTREAM @ $(git ls-remote "$UPSTREAM" HEAD | cut -c1-7)  N=$N  go flags: ${TRIM:-none}  kernel: $(uname -r)  cpus: $(nproc)  fs: $(findmnt -no FSTYPE "$FS")  git: $(git --version | cut -d' ' -f3)  pnpm: $(pnpm --version)  tingly-fuse: $(git -C "$(dirname "$0")" rev-parse --short HEAD)" | tee -a "$OUT/spinup-env.txt"
    for m in $METHODS; do run_method "$m"; done
    ;;
  *) sed -n '2,22p' "$0"; exit 2 ;;
esac
