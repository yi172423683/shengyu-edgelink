package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// statsSocketPrefix/_suffix 与 haproxy.StatsSocketFileName 的命名保持一致。
// 这里用字面量解析（而不是反查），是因为本函数还要**容忍**无法解析的文件名：
// 解析不出来的东西我们不知道怎么处理，那就一律不碰。
const (
	statsSocketPrefix = "stats-v"
	statsSocketSuffix = ".sock"
)

// StatsSocketPrune 一次"旧统计套接字清理"的结果。
//
// 三类分别对应三种处置，缺任何一类运维都说不清"到底动了什么"：
//   - Removed：确实删掉了（已确认无进程使用）；
//   - Kept：保留，并**说明为什么**（有人在用 / 判不出来 / 当前版本）；
//   - Problems：想删但删失败 —— 只告警，**绝不影响发布结果**。
type StatsSocketPrune struct {
	Dir      string   `json:"dir"`
	Scanned  int      `json:"scanned"`
	Removed  []string `json:"removed,omitempty"`
	Kept     []string `json:"kept,omitempty"`
	Problems []string `json:"problems,omitempty"`
	// Note 一句人话总结，界面与审计直接用。
	Note string `json:"note,omitempty"`
}

// StatsSocketPruner 是数据面提供的**可选**能力：清理无主的旧统计套接字。
//
// 为什么不实现它的数据面也要能跑：纯 Go 数据面（测试替身）没有 /run 下的套接字，
// 这条能力对它**不适用** —— 调用方要按"不适用"处理，而不是"已通过"。
// （同 FileSetInspector 的处理方式：可选能力缺失必须显式记成"不适用"。）
type StatsSocketPruner interface {
	PruneStaleStatsSockets(ctx context.Context) StatsSocketPrune
}

// probeUnixSocket 真实探测：能建立连接 = 有 worker 在应答。
//
// 这里的三分法决定了"敢不敢删"，所以必须严格区分：
//
//	连上                     ⇒ 有人在用（**包括正在优雅退出的旧 worker**）⇒ 不删；
//	ECONNREFUSED / 文件不存在 ⇒ 确实无人监听 ⇒ 可删；
//	其它（超时、EACCES…）     ⇒ **判不出来** ⇒ 不删。
//
// 最后一条尤其重要：统计套接字的属主是运行 HAProxy 的用户，本进程可能因权限被拒 ——
// 那是"我看不见"，不是"它不存在"。宁可留一个垃圾文件，
// 也不能凭猜测删掉一个可能还在服务的 worker 的 socket：
// 删掉它不影响转发，但会让 Stats() 少一个候选、让排障时看到的现场与事实不符。
func probeUnixSocket(ctx context.Context, path string) (alive, certain bool) {
	d := net.Dialer{Timeout: 300 * time.Millisecond}
	conn, err := d.DialContext(ctx, "unix", path)
	if err == nil {
		_ = conn.Close()
		return true, true
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, true
	case errors.Is(err, syscall.ECONNREFUSED):
		return false, true
	}
	// 兜底：不同平台/包装层对"连接被拒"的措辞不完全一致，
	// 而这一条判错方向的后果是"删掉一个还在用的 socket"，所以宁可多认一层。
	if s := strings.ToLower(err.Error()); strings.Contains(s, "connection refused") {
		return false, true
	}
	return false, false
}

// PruneStaleStatsSockets 清理"已确认无进程使用"的旧统计套接字。
//
// 规则（按保守程度排序，前一条否决后一条）：
//
//	① 只处理 `stats-v<N>.sock` 形状的文件；解析不出来的不碰；
//	② **当前生效版本**的套接字一律保留 —— reload 窗口里它可能暂时连不上，
//	   删它没有收益（下一轮还会重建），却会让 Stats() 的兜底候选少一个；
//	③ 能连上的一律保留（有 worker 在应答，含正在优雅退出的旧 worker）；
//	④ 判不出来的保留（见 probeUnixSocket 的说明）；
//	⑤ 以上都不成立（明确无人监听）才删。
//
// 失败只进 Problems，**不返回 error**：这个动作是"顺手打扫"，
// 它出问题不该影响任何一次成功发布的结果。
func (h *HAProxy) PruneStaleStatsSockets(ctx context.Context) StatsSocketPrune {
	res := StatsSocketPrune{Dir: strings.TrimSpace(h.StatsSocketDir)}
	dir := res.Dir
	if dir == "" {
		res.Note = "未启用按版本区分的统计套接字（StatsSocketDir 为空），无需清理"
		return res
	}
	entries, err := h.readDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			res.Note = "统计套接字目录不存在，无需清理"
			return res
		}
		res.Problems = append(res.Problems, "读取统计套接字目录失败: "+err.Error())
		res.Note = "统计套接字清理未能执行（读不到目录）"
		return res
	}
	probe := h.probeSock
	if probe == nil {
		probe = probeUnixSocket
	}
	curVer, _ := h.ActiveVersion(ctx)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, statsSocketPrefix) || !strings.HasSuffix(name, statsSocketSuffix) {
			continue
		}
		res.Scanned++
		path := filepath.Join(dir, name)
		if v, ok := statsSocketVersion(name); ok && v == curVer {
			res.Kept = append(res.Kept, name+"（当前生效版本，保留）")
			continue
		}
		alive, certain := probe(ctx, path)
		if alive {
			res.Kept = append(res.Kept, name+"（有 worker 在应答，可能是正在优雅退出的旧 worker）")
			continue
		}
		if !certain {
			res.Kept = append(res.Kept, name+"（无法确认是否仍有进程使用，保守保留）")
			continue
		}
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			res.Problems = append(res.Problems, fmt.Sprintf("删除 %s 失败: %v", name, rerr))
			continue
		}
		res.Removed = append(res.Removed, name)
	}
	sort.Strings(res.Removed)
	sort.Strings(res.Kept)
	res.Note = fmt.Sprintf("扫描 %d 个统计套接字：清理 %d 个（已确认无进程使用），保留 %d 个",
		res.Scanned, len(res.Removed), len(res.Kept))
	if len(res.Problems) > 0 {
		res.Note += fmt.Sprintf("；另有 %d 项未能完成（不影响转发，也不影响本次发布结果）", len(res.Problems))
	}
	return res
}

// statsSocketVersion 从 `stats-v<N>.sock` 解析出版本号；解析不出来返回 ok=false。
func statsSocketVersion(name string) (int, bool) {
	if !strings.HasPrefix(name, statsSocketPrefix) || !strings.HasSuffix(name, statsSocketSuffix) {
		return 0, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, statsSocketPrefix), statsSocketSuffix)
	n, err := strconv.Atoi(body)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
