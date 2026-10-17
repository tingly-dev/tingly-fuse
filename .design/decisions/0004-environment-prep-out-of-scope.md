# 0004 环境准备不归 tingly-fuse

- 状态：已采纳
- 日期：2026-10-17

## 背景

基准测试发现，overlay 下 `pnpm install` 因为无法跨挂载点硬链接，每个会话会整份复制依赖，约 800MB。把依赖预装进基础层后降到 0MB、约 2.4 秒。一度考虑让 tingly-fuse 在 spawn 时按 commit 预装依赖（`pnpm install`、`go mod download` 等）。

## 决定

不做。装依赖、构建、刷新环境，都发生在 tingly-fuse 之前，由使用者自己准备。tingly-fuse 只负责：拿到一个现成的底层之后，让多个 agent 互不干扰。

issue #1 因此收窄为：接收一个已准备好的目录，作为只读底层使用，它要做的只有拍只读快照、处理 `.git`、记录来源。

## 理由

- 环境怎么准备因项目而异，tingly-fuse 不该知道 pnpm、Go 或任何具体工具链。
- 职责清楚：fuse 解决"后续"的隔离与共享问题。

## 后果

- 不检查 lockfile，不判断环境是否过期，不安装任何东西。
- 使用者想避免 overlay 下 pnpm 的复制代价，需要自己把依赖放进底层。
