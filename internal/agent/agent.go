// Package agent 实现节点侧运行时：日志接收、解析入库、数据面托管。
//
// 它同时服务于两种部署形态（需求 §二.5：管理平台和节点允许同机部署）：
//
//	同机部署：shengyu-edgelink-server 直接内嵌本包，数据面进程在同一台机器上；
//	远程节点：独立的 shengyu-agent 进程使用本包的接收与上报能力，通过 WSS/HTTPS 回传。
//
// 两者共用日志接收代码的意义在于：**日志格式与解析逻辑只有一份**。
// 如果同机与远程各写一套，迟早会出现"本机查得到、远程查不到"这种最难查的问题。
package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shengyu/edgelink/internal/logparse"
	"github.com/shengyu/edgelink/internal/logstore"
)

// Ingestor 把一行 HAProxy 风格的日志解析后写入分片日志库。
//
// 设计要点：
//   - **解析失败也必须入库**（ParseOK=false + 保留原文）。
//     直接丢弃的结果是"运维知道有问题，但永远看不到问题长什么样"。
//   - 落盘时间（RecvTS）与连接建立时间（AcceptTS）分开记录。
//     TCP 日志只有会话结束才产生，一条挂了 6 小时的连接，它的 accept 时间可能是 6 小时前，
//     而它是刚刚才被采集到的。这两个时间混用会让"日志延迟"这个问题无法被观测。
type Ingestor struct {
	Store    *logstore.Store
	Resolver logparse.Resolver
	// NodeID 用于给缺失 node 字段的日志补上归属。
	NodeID string

	// bootID 是本采集器实例的启动标识，参与幂等键。
	//
	// 为什么不能只用自增 seq：seq 只存在内存里，接收器一重启就从 1 重新开始。
	// 而日志表的幂等键是 (node_id, boot_id, seq) 且用 INSERT OR IGNORE ——
	// 若没有 boot_id，重启后新写入的连接会和重启前的老序号撞上，
	// **被当成重复记录静默丢弃**：转发一切正常，但日志少了一整段。
	// 加上 boot_id 后：同一批次重传仍然幂等（同 boot_id + seq），
	// 而不同启动之间互不干扰。
	bootID string

	seq        atomic.Int64
	parseErrs  atomic.Int64
	ingested   atomic.Int64
	duplicat   atomic.Int64
	dropped    atomic.Int64
	lastErr    atomic.Value // string
	lastIngest atomic.Value // time.Time
}

// NewIngestor 创建摄入器。
func NewIngestor(st *logstore.Store, res logparse.Resolver, nodeID string) *Ingestor {
	return &Ingestor{Store: st, Resolver: res, NodeID: nodeID, bootID: newBootID()}
}

// newBootID 生成启动标识。用随机值即可 —— 只要求"两次启动几乎不可能相同"。
func newBootID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand 失败不阻断日志采集：退化成"按纳秒构造"，碰撞概率依然可忽略。
		return "t" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// BootID 返回本实例的启动标识（诊断用）。
func (i *Ingestor) BootID() string { return i.bootID }

// Stats 摄入统计，供节点上报给管理面（需求 §八：记录日志丢弃、采集延迟，避免静默丢失）。
type IngestStats struct {
	Ingested   int64  `json:"ingested"`
	Duplicated int64  `json:"duplicated"`
	ParseErrs  int64  `json:"parse_errors"`
	Dropped    int64  `json:"dropped"`
	LastError  string `json:"last_error,omitempty"`
	BootID     string `json:"boot_id,omitempty"`
}

// IngestStats 返回累计统计。
func (i *Ingestor) IngestStats() IngestStats {
	s := IngestStats{
		Ingested:   i.ingested.Load(),
		Duplicated: i.duplicat.Load(),
		ParseErrs:  i.parseErrs.Load(),
		Dropped:    i.dropped.Load(),
		BootID:     i.bootID,
	}
	if v, ok := i.lastErr.Load().(string); ok {
		s.LastError = v
	}
	return s
}

// IngestLine 处理一行日志（**同步**写盘后才返回）。
//
// 它**永不返回错误**：日志系统故障不得阻塞业务转发（需求 §八）。
// 出错时计数并记下最后一条错误，由管理面展示"采集异常"，而不是往上抛。
//
// 为什么保留"逐行同步"这个入口：同机部署的诊断链路、以及端到端验收，
// 都需要"写完立刻可查"的强保证。高吞吐场景走 Batcher（攒批），
// 两者最终都调用 IngestLines —— 解析与入库逻辑只有一份。
func (i *Ingestor) IngestLine(line string) {
	_, _ = i.IngestLines([]string{line})
}

// IngestLines 处理一批日志。
//
// 这是**批量写入**的落点：一次调用 = 一次事务 = 一次连接获取。
// 关键是"解析逐行、入库整批"：解析失败不影响同批其它行入库
// （失败行仍然入库，ParseOK=false 并保留原文）。
//
// 返回的错误只表示"整批写盘失败"；单行解析失败不产生错误，
// 只计入 ParseErrs 与 IngestStats，避免一行坏数据把整批日志拖下水。
func (i *Ingestor) IngestLines(lines []string) (logstore.IngestResult, error) {
	var res logstore.IngestResult
	if len(lines) == 0 {
		return res, nil
	}
	rows := make([]logstore.ConnRow, 0, len(lines))
	now := time.Now()
	for _, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		rec, err := logparse.Parse([]byte(line), i.Resolver)
		if err != nil {
			i.parseErrs.Add(1)
			i.lastErr.Store(err.Error())
		}
		if rec.NodeID == "" {
			rec.NodeID = i.NodeID
		}
		rows = append(rows, logstore.ConnRow{
			BootID: i.bootID, Seq: i.seq.Add(1), RecvTS: now, Rec: rec,
		})
	}
	if len(rows) == 0 {
		return res, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ing, ierr := i.Store.Ingest(ctx, rows)
	if ierr != nil {
		i.dropped.Add(int64(len(rows)))
		i.lastErr.Store(ierr.Error())
		return res, ierr
	}
	// 按**实际落库结果**计数，而不是"调用没报错就算写入成功"。
	// 原先的写法把被 INSERT OR IGNORE 忽略掉的行也记成 ingested，
	// 界面上会显示"采集正常"，而实际上日志正在被丢。
	i.ingested.Add(int64(ing.Accepted))
	i.duplicat.Add(int64(ing.Duplicated))
	i.lastIngest.Store(time.Now())
	return ing, nil
}

// LastIngestAt 最近一次成功入库的时间（零值表示还没有入库过）。
//
// 用途是"采集是否卡住"这个问题的可观测性：只报累计条数看不出"已经 5 分钟没写进去了"。
func (i *Ingestor) LastIngestAt() time.Time {
	if v, ok := i.lastIngest.Load().(time.Time); ok {
		return v
	}
	return time.Time{}
}

// ============================== Unix 数据报日志接收 ==============================

// UnixDgramReceiver 接收 HAProxy 通过 `log` 指令发来的日志。
//
// 为什么用 SOCK_DGRAM 而不是 SOCK_STREAM：
// HAProxy 的 log 指令在生产配置下就是发数据报。用流式套接字会碰到"消息边界"问题——
// 多条日志可能被合并成一个读；数据报天然保持一行一条。若该版本必须用流式，
// Go 侧按行切分同样能工作，所以这里两种都支持。
type UnixDgramReceiver struct {
	Path    string
	Network string // "unixgram"（默认）或 "unix"
	// Buffer 单条日志的最大长度。HAProxy 默认会截断到 1024 字节左右，
	// 但配了长 log-format 时会更长，所以给足余量。
	Buffer int

	conn    net.PacketConn
	ln      net.Listener
	mu      sync.Mutex
	stopped atomic.Bool
	// Drops 记录因缓冲区不足或解析前置校验失败而丢弃的条数。
	drops atomic.Int64
}

// Listen 绑定套接字。会先清理残留的 socket 文件（进程被 kill 时不会自己清理）。
func (u *UnixDgramReceiver) Listen() error {
	if u.Path == "" {
		return errors.New("agent: 未配置日志套接字路径")
	}
	network := u.Network
	if network == "" {
		network = "unixgram"
	}
	// 残留 socket 文件会让 bind 失败，且报错信息（address already in use）会误导排查方向。
	// 只有当文件存在**且确实没有进程在监听**时才删除，避免误删正在使用的套接字。
	if fi, err := os.Lstat(u.Path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("agent: %s 已存在且不是套接字文件，拒绝覆盖", u.Path)
		}
		if c, derr := net.DialTimeout(network, u.Path, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			return fmt.Errorf("agent: %s 已有进程在监听，请先停止它", u.Path)
		}
		if rerr := os.Remove(u.Path); rerr != nil {
			return fmt.Errorf("agent: 清理残留套接字失败: %w", rerr)
		}
	}
	if network == "unixgram" {
		c, err := net.ListenPacket(network, u.Path)
		if err != nil {
			return fmt.Errorf("agent: 绑定 %s 失败: %w", u.Path, err)
		}
		u.conn = c
		return nil
	}
	ln, err := net.Listen(network, u.Path)
	if err != nil {
		return fmt.Errorf("agent: 绑定 %s 失败: %w", u.Path, err)
	}
	u.ln = ln
	return nil
}

// Serve 阻塞接收直到 ctx 结束或 Stop 被调用。
func (u *UnixDgramReceiver) Serve(ctx context.Context, sink func(string)) error {
	bufSize := u.Buffer
	if bufSize <= 0 {
		bufSize = 64 * 1024
	}
	go func() {
		<-ctx.Done()
		u.stop()
	}()
	if u.conn != nil {
		buf := make([]byte, bufSize)
		for {
			n, _, err := u.conn.ReadFrom(buf)
			if err != nil {
				if u.stopped.Load() {
					return nil
				}
				u.drops.Add(1)
				continue
			}
			sink(string(buf[:n]))
		}
	}
	// 流式套接字：每来一个连接起一个 goroutine 按行扫。
	// HAProxy 在流式模式下可能长期保持连接，因此不能"收完一个连接再收下一个"。
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := u.ln.Accept()
		if err != nil {
			if u.stopped.Load() {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer c.Close()
			sc := bufio.NewScanner(c)
			sc.Buffer(make([]byte, 0, bufSize), bufSize)
			for sc.Scan() {
				sink(sc.Text())
			}
		}(c)
	}
}

func (u *UnixDgramReceiver) stop() {
	u.stopped.Store(true)
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.conn != nil {
		_ = u.conn.Close()
	}
	if u.ln != nil {
		_ = u.ln.Close()
	}
}

// Stop 主动停止。
func (u *UnixDgramReceiver) Stop() { u.stop() }

// Drops 因接收异常丢弃的条数。
func (u *UnixDgramReceiver) Drops() int64 { return u.drops.Load() }
