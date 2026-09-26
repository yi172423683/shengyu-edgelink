module github.com/shengyu/edgelink

go 1.22

// 核心逻辑包（model / validate / haproxy / logparse / logstore / diagnose / publish /
// probe / dns / helper / redact / id）**只用 Go 标准库 + database/sql**：
//   - 便于审计（代理+私有化交付场景，第三方代码越少越好）；
//   - 交叉编译零依赖；
//   - 降低"某个依赖被投毒导致中转平台被控"这类供应链风险。
//
// 唯一的第三方依赖是纯 Go（无 CGO）的 SQLite 驱动，且**只在 internal/db 里被注册**。
// 这样 `go test ./internal/...` 在离线环境（GOPROXY=off + 模块缓存命中）也能跑通，
// 交叉编译到 linux/amd64 时也不需要 C 工具链。
require modernc.org/sqlite v1.34.1

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.22.0 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
)
