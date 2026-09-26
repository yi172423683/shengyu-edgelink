package haproxy

import "time"

// ManifestFileName 版本目录里"这一版由哪些文件组成"的清单文件名。
//
// 它同时会被发布到 current/：`cat current/manifest.json` 就能回答
// "现在生效的这套配置**应该**有哪几个文件"，而不必去猜哪些残留是生效的。
const ManifestFileName = "manifest.json"

// VersionManifest 一版配置的完整文件清单。
//
// 存在的三个理由，全都围绕"current/ 到底应该有哪几个文件"：
//
//	① 发布前清理：current/ 里不属于本清单的文件必须删掉。
//	   版本目录是**逐版增删**的 —— 某一版多了 sni_allow_9443.lst，
//	   回滚到没有这条入口的旧版后那个文件会滞留在 current/，
//	   让 `ls current/` 显示一个"看起来仍在生效"的允许清单。
//	② 回滚核对：回滚后 current/ 必须与目标版本的清单逐字节一致。
//	③ 人工排查：清单本身就是"这一版是什么"的权威说明。
//
// 清单与自身的关系（必须写死，否则读方无法还原集合）：
// Files 列出**除 VERSION 与 manifest.json 自身之外**的全部文件。
// 完整集合 = Files ∪ {manifest.json}（current/ 再多一个 VERSION）。
type VersionManifest struct {
	Version         int       `json:"version"`
	NodeID          string    `json:"node_id"`
	ContentHash     string    `json:"content_hash"`
	RendererVersion string    `json:"renderer_version"`
	CreatedAt       time.Time `json:"created_at"`
	// Files 已排序，便于人工比对与逐字节 diff。
	Files []string `json:"files"`
}
