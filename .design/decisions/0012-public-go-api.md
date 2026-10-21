# 0012 对外的 Go API：模块根目录的 `tinglyfuse` 包

- 状态：已采纳
- 日期：2026-10-20

## 背景

所有功能都在 `internal/` 下，只有命令行能用。YZ 要求这些功能都能被程序调用，而不只是 CLI。

## 决定

- 模块根目录（`github.com/tingly-dev/tingly-fuse`）新增 `tinglyfuse` 包：`Open`/`New` 得到 `Manager`，类型（`Manager`、`SpawnOptions`、`AddOptions`、`Repo`、`Session`、`BaseInfo`、`BaseHealth`）是 `internal/` 里对应类型的别名，另有 `Backends`、`DefaultBackend`、`CheckBackend` 和会话内环境变量名。导入它就注册了本平台的所有后端。
- 命令行只导入这个包，不再直接用 `internal/`：命令行能做的，API 一定能做。
- `CheckBackend` 新建并进入一个临时视图，告诉调用者某个后端在本机能不能用（例如 overlay 需要 mount 和非特权 user namespace）。

## 理由

- 别名让 API 和实现是同一套类型，不用维护两份结构和转换；`internal/` 的包结构仍可自由调整，只要别名指向的东西还在。
- 命令行作为 API 的第一个使用者，保证 API 完整。

## 被放弃的选项

- **HTTP/JSON 服务。** 需要常驻进程和鉴权，现在没有调用方需要跨进程；以后需要时可以在这个包上包一层。
- **把 `internal/manager` 直接改成公开包。** 会把 `repo`、`session`、`backend` 都带成公开包，承诺的接口面更大。

## 后果

- 别名类型的字段和方法就是公开接口，改 `internal/` 里这些类型时要当作 API 变更对待。
- `TestAPI` 只用公开包走完整个生命周期，在每个可用后端上跑；CI 里以 root 再跑一遍，保证 overlay 一定执行。
