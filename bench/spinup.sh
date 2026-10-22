#!/usr/bin/env bash
# 从零给 N 个 agent 各开一个工作区，对比几种做法。一次性成本（clone、建 base、准备目录）也算在内。
#
# 两个维度分开测，互不影响：
#
#   deps   工作区 × 依赖：每个 agent 创建工作区、git status、pnpm install、前端构建（vite build）
#     DEPS=own-store     每个 agent 自己的 pnpm store，联网安装（什么都不共享）
#     DEPS=shared-store  共用一个预热的 pnpm store，各自一份 node_modules（离线）
#     DEPS=shared-nm     frontend/node_modules 所有 agent 共用一份
#                        （clone/worktree 用 bind mount，tf 用 --share，机制相同）
#     DEPS=in-base       依赖预装在只读 base 里（只有 tf：git worktree add --from-dir）
#
#   build  工作区 × Go 构建缓存：每个 agent 创建工作区、go build ./cli/tingly-box
#     BUILD=private          每个 agent 自己的模块缓存和构建缓存（什么都不共享，联网下载模块）
#     BUILD=modcache         共用预热的模块缓存，构建缓存各自一份
#     BUILD=shared           模块缓存和构建缓存都共用（构建缓存在每组开始前清空）
#     BUILD=shared-trimpath  同 shared，加 go build -trimpath（路径不进缓存键）
#
# 工作区做法（METHODS）：
#   clone     每个 agent 从上游完整 git clone --recurse-submodules（走网络）
#   worktree  上游 clone 一个主仓库；每个 agent git worktree add + git submodule update --init
#   tf        tingly-fuse：git clone 一次；每个 agent git worktree add（overlay 后端）
#
# 用法：bench/spinup.sh setup; bench/spinup.sh deps; bench/spinup.sh build
# 环境变量：BENCH_FS（默认 /mnt/bench，专用文件系统，磁盘用 statfs 增量，缓存也放在这里）、
#           BENCH_N（默认 3）、BENCH_METHODS、BENCH_DEPS、BENCH_BUILDS、BENCH_UPSTREAM
# 需要 root（bind mount、overlay）。
set -euo pipefail

UPSTREAM=${BENCH_UPSTREAM:-https://github.com/tingly-dev/tingly-box}
FS=${BENCH_FS:-/mnt/bench}
ROOT=$FS/spinup
STORE=$FS/pnpm-store          # 预热的 pnpm store，与工作区同一文件系统，pnpm 才能硬链接
MODCACHE=$FS/gomod            # 预热的 Go 模块缓存
METHODS=${BENCH_METHODS:-clone worktree tf}
DEPS_VARIANTS=${BENCH_DEPS:-own-store shared-store shared-nm in-base}
BUILD_VARIANTS=${BENCH_BUILDS:-private modcache shared shared-trimpath}
N=${BENCH_N:-3}
OUT=${BENCH_OUT:-$PWD/bench-out}
TF=${TINGLY_FUSE:-tingly-fuse}

export CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0 GOTOOLCHAIN=auto GOFLAGS=-modcacherw
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=advice.detachedHead GIT_CONFIG_VALUE_0=false
export GIT_AUTHOR_NAME=bench GIT_AUTHOR_EMAIL=bench@example.com
export GIT_COMMITTER_NAME=bench GIT_COMMITTER_EMAIL=bench@example.com

log() { echo "[$(date +%T)] $*" >&2; }
now() { date +%s.%N; }
used() { sync -f "$FS"; df --output=used -B1 "$FS" | tail -1; }
mb() { awk -v b="$1" 'BEGIN{printf "%.1f", b/1048576}'; }

setup() {
  mkdir -p "$STORE" "$MODCACHE"
  local tmp=$FS/warm
  rm -rf "$tmp"
  git clone -q --recurse-submodules "$UPSTREAM" "$tmp"
  (cd "$tmp/frontend" && pnpm install --frozen-lockfile --store-dir "$STORE" >/dev/null)
  (cd "$tmp" && GOMODCACHE=$MODCACHE go mod download)
  rm -rf "$tmp"
  log "setup done"
}

TFHOME=$ROOT/tf
tf() { TINGLY_FUSE_HOME=$TFHOME "$TF" "$@"; }
in_dir() { (cd "$1" && bash -c "$2"); }

# 共享 node_modules：clone/worktree 把同一个目录 bind mount 到每个工作区（和 tf --share 同一机制）。
share_nm() {
  [ "$DEPS" = shared-nm ] || return 0
  mkdir -p "$ROOT/nm" "$ROOT/$1/frontend/node_modules"
  mount --bind "$ROOT/nm" "$ROOT/$1/frontend/node_modules"
}

# 每种做法：<m>_once（一次性准备）、<m>_create <id>、<m>_in <id> <cmd>
clone_once() { :; }
clone_create() { git clone -q --recurse-submodules "$UPSTREAM" "$ROOT/$1"; share_nm "$1"; }
clone_in() { in_dir "$ROOT/$1" "$2"; }

worktree_once() { git clone -q --recurse-submodules "$UPSTREAM" "$ROOT/main"; }
worktree_create() {
  git -C "$ROOT/main" worktree add -q -b "agent/$1" "$ROOT/$1" HEAD
  git -C "$ROOT/$1" submodule update --init -q
  share_nm "$1"
}
worktree_in() { in_dir "$ROOT/$1" "$2"; }

tf_once() {
  case $DEPS in
    shared-nm) tf git clone --share frontend/node_modules "$UPSTREAM" tingly-box >/dev/null ;;
    in-base)
      git clone -q --recurse-submodules "$UPSTREAM" "$ROOT/prep"
      in_dir "$ROOT/prep" "$PNPM"
      tf git clone "$UPSTREAM" tingly-box >/dev/null
      tf git worktree add tingly-box --from-dir "$ROOT/prep" --id first >/dev/null
      tf drop first
      rm -rf "$ROOT/prep" ;;  # base 是快照，准备目录用完即可删
    *) tf git clone "$UPSTREAM" tingly-box >/dev/null ;;
  esac
}
tf_create() {
  if [ "$DEPS" = in-base ]; then
    tf git worktree add tingly-box --base "$(ls "$TFHOME/repos/tingly-box/bases" | grep -m1 '^dir-')" --id "$1" >/dev/null
  else
    tf git worktree add tingly-box --id "$1" >/dev/null
  fi
}
tf_in() { tf exec "$1" -- bash -c "$2"; }

record() {  # part variant method agent step seconds disk_bytes
  printf '%s,%s,%s,%s,%s,%.3f,%s\n' "$1" "$2" "$3" "$4" "$5" "$6" "$(mb "$7")" | tee -a "$CSV"
}

measure() {  # agent step command...
  local a=$1 step=$2; shift 2
  local u0 t0 t1 u1
  u0=$(used); t0=$(now)
  "$@" >>"$OUT/spinup-log.txt" 2>&1 || { log "FAILED: $PART $VARIANT $M $a $step"; tail -20 "$OUT/spinup-log.txt" >&2; return 1; }
  t1=$(now); u1=$(used)
  record "$PART" "$VARIANT" "$M" "$a" "$step" "$(awk -v a="$t0" -v b="$t1" 'BEGIN{print b-a}')" $((u1 - u0))
}

cleanup() {
  if [ -d "$TFHOME/sessions" ]; then
    for s in "$TFHOME"/sessions/*/; do [ -d "$s" ] && tf drop "$(basename "$s")" >/dev/null 2>&1 || true; done
  fi
  { findmnt -rno TARGET | grep "^$ROOT/" || true; } | sort -r | while read -r t; do umount "$t"; done
  rm -rf "$ROOT"
  fstrim "$FS" 2>/dev/null || true   # 回环盘：把删掉的块还给宿主
}

pnpm_cmd() {  # agent
  case $DEPS in
    own-store) echo "cd frontend && pnpm install --frozen-lockfile --store-dir $ROOT/store-$1" ;;
    *) echo "$PNPM" ;;
  esac
}

go_env() {  # agent
  case $BUILD in
    private) echo "GOMODCACHE=$ROOT/gomod-$1 GOCACHE=$ROOT/gocache-$1" ;;
    modcache) echo "GOMODCACHE=$MODCACHE GOCACHE=$ROOT/gocache-$1" ;;
    *) echo "GOMODCACHE=$MODCACHE GOCACHE=$ROOT/gocache" ;;
  esac
}

PNPM="cd frontend && pnpm install --frozen-lockfile --offline --store-dir $STORE"
GITST="git status --porcelain >/dev/null"

run() {  # part variant method
  PART=$1 VARIANT=$2 M=$3
  if [ "$PART" = deps ] && [ "$VARIANT" = in-base ] && [ "$M" != tf ]; then return; fi
  # 可续跑：已完成的组合跳过，没跑完的组合先删掉残留的行
  if grep -q "^$PART,$VARIANT,$M,all,total," "$CSV"; then return; fi
  sed -i "/^$PART,$VARIANT,$M,/d" "$CSV"
  DEPS=shared-store BUILD=shared
  if [ "$PART" = deps ]; then DEPS=$VARIANT; else BUILD=$VARIANT; fi
  cleanup; mkdir -p "$ROOT"
  log "== $PART $VARIANT $M"
  local u0 t0; u0=$(used); t0=$(now)
  measure setup once "${M}_once"
  for i in $(seq 1 "$N"); do
    measure "a$i" create "${M}_create" "a$i"
    if [ "$PART" = deps ]; then
      measure "a$i" git-status "${M}_in" "a$i" "$GITST"
      measure "a$i" pnpm-install "${M}_in" "a$i" "$(pnpm_cmd "a$i")"
      measure "a$i" frontend-build "${M}_in" "a$i" "cd frontend && pnpm build"
    else
      local trim=; [ "$BUILD" = shared-trimpath ] && trim=-trimpath
      measure "a$i" go-build "${M}_in" "a$i" "env $(go_env "a$i") go build $trim -o /dev/null ./cli/tingly-box"
    fi
  done
  record "$PART" "$VARIANT" "$M" all total "$(awk -v a="$t0" -v b="$(now)" 'BEGIN{print b-a}')" $(( $(used) - u0 ))
  cleanup
}

case ${1:-} in
  setup) setup ;;
  deps|build)
    mkdir -p "$OUT"
    CSV=$OUT/spinup.csv
    [ -f "$CSV" ] || echo "part,variant,method,agent,step,seconds,disk_mb" >"$CSV"
    echo "date: $(date -u +%FT%TZ)  part: $1  upstream: $UPSTREAM @ $(git ls-remote "$UPSTREAM" HEAD | cut -c1-7)  N=$N  kernel: $(uname -r)  cpus: $(nproc)  fs: $(findmnt -no FSTYPE "$FS")  git: $(git --version | cut -d' ' -f3)  tingly-fuse: $(git -C "$(dirname "$0")" rev-parse --short HEAD)" | tee -a "$OUT/spinup-env.txt"
    if [ "$1" = deps ]; then variants=$DEPS_VARIANTS; else variants=$BUILD_VARIANTS; fi
    for v in $variants; do for m in $METHODS; do run "$1" "$v" "$m"; done; done
    ;;
  *) sed -n '2,30p' "$0"; exit 2 ;;
esac
