package haproxy

import (
	"strconv"

	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/model"
)

// 本文件把"渲染器给对象起的名字"导出成公开函数。
//
// 为什么必须导出：连接日志里记的是 HAProxy 的对象名（%b / %s），
// 平台要靠这些名字反查业务与源站。测试替身数据面（internal/testplane）也必须用**完全一样**的
// 命名，否则它在自动化测试里产生的日志就不是平台真实会遇到的那一种。把命名规则集中在这里。

// BackendName 一条路由对应的 backend 名。
func BackendName(v model.RouteView) string { return backendName(v) }

// ServerName backend 里源站 server 的名字。当前每 backend 只有一个源站，固定 s1。
func ServerName(v model.RouteView) string { _ = v; return "s1" }

// SNIFrontendName 共享 SNI 入口的 frontend 名。
func SNIFrontendName(port int) string { return "fe_sni_" + strconv.Itoa(port) }

// TCPFrontendName TCP 端口转发的 frontend 名。
func TCPFrontendName(businessID string) string { return "fe_tcp_" + id.Handle(businessID) }

// UnmatchedBackendName 未匹配 SNI 的兜底 backend 名（它的 server 故意指向必然不可达地址，
// 使连接以明确的错误终止，而不是挂住或被误认为"已进入业务"）。
func UnmatchedBackendName(port int) string { return unmatchedBackendName(port) }
