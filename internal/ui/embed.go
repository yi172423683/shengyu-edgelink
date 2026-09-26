// Package ui 提供随二进制分发的管理台页面。
//
// 为什么用 go:embed 把页面打进二进制（而不是另起一个 Vue/React 前端）：
//   - go.mod 明确约束「只用 Go 标准库」，embed 是标准库，零额外依赖；
//   - 单文件分发：装完 shengyu-edgelink-server 就有界面，不用再配 nginx 静态目录与构建流水线；
//   - 页面是原生 HTML + 原生 JS，没有框架，也没有构建步骤。
//
// 页面仍走同源的 /api/* 接口：管理面默认只绑 127.0.0.1，
// 需要对外时由外层反代提供 HTTPS（与既有部署约定一致）。
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html
var content embed.FS

// FS 暴露嵌入的静态文件（便于测试与将来扩展）。
func FS() fs.FS { return content }

// Handler 返回管理台页面处理器。
//
// 页面在编译期就打进二进制，ReadFile 理论上不会失败；真失败了就明确报 500，
// 而不是返回一个空白页让人以为是接口坏了。
func Handler() http.Handler {
	b, err := content.ReadFile("index.html")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "管理台页面未打包进二进制："+err.Error(), http.StatusInternalServerError)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// 页面自带版本号与后端一致，不缓存避免升级后拿到旧界面。
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
}
