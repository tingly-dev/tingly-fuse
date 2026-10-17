# 0003 对象库共享，基础层通过 alternates 引用

- 状态：已采纳
- 日期：2026-10-17

## 背景

最初的 overlay 演示里，如果 `.git/objects` 在 overlay 的只读底层中，git 查询对象时会刷新 pack 文件的 mtime，这会触发 overlay 把整个 pack 复制到该会话的差量层。tingly-box 的 pack 约 19MB，每个会话都要多出这么多。

## 决定

- 每个登记的仓库只有一份 bare 镜像（`repos/<name>/objects.git`），所有对象只存在这里。
- 每个 commit 一份只读基础 checkout（`bases/<sha>/`），用 `clone --shared` 建立，`.git` 只带 `objects/info/alternates` 指向镜像，自己不存对象。
- 子模块同样：每个子模块 URL 一份 bare 镜像（`modules/<hash>.git`），基础 checkout 里的子模块仓库也只带 alternates。origin 保持真实 URL，agent 在视图里仍可正常 fetch 和 push。

## 理由

- 底层里没有 pack 可被 freshen，会话的差量层只含真正改动的内容，实测每会话约 100KB，copy 后端是 44MB。
- 一个 commit 的 base 被所有从它出发的会话共用，创建会话时不再复制对象。
- 子模块不处理的话 tingly-box 无法 `go build`（`go.mod` 用 replace 指向 `libs/` 下的子模块），处理后在 overlay 会话里完整构建通过。

## 后果

- 镜像是唯一的对象来源，不能随便删；`repo remove` 才会连同所有 base 一起清掉。
- `EnsureBase` 要并发安全：先在临时目录建好再 rename，先到先得。
