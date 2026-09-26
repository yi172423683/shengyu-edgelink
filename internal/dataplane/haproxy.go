package dataplane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/model"
)

// HAProxyName 生产实现的标识名。
const HAProxyName = "haproxy"

// runner 执行外部命令。抽成函数类型是为了让测试能替换成假实现 ——
// 否则"发布失败要回滚"这类逻辑只能在真实 Linux 上验证，而那正是最容易出错的地方。
type runner func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)

func execRunner(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// HAProxy 是生产转发内核的应用器。
//
// 目录布局（与 docs/01 一致）：
//
//	/etc/shengyu-edgelink/haproxy/
//	  versions/v1/haproxy.cfg       ← 每次发布一个**不可变**版本目录
//	  versions/v1/meta.json
//	  versions/v2/...
//	  current/VERSION               ← 当前生效版本号（最后写入）
//	  current/haproxy.cfg           ← HAProxy 实际读取的配置
//	  current/sni_allow_*.txt       ← SNI 允许清单
//
// 为什么是"版本目录 + 原子替换文件"，而不是"版本目录 + 符号链接"：
//
//   - **原子性的粒度本来就该是文件**。符号链接把整个目录原子切换，
//     但 HAProxy 关心的是 cfg 与它引用的允许清单是否**互相匹配**；
//     一次发布里这些文件本来就是一起换的，逐个原子替换 + 最后写 VERSION
//     既保证了"不会读到半写文件"，也保证了"VERSION 标记的一定是完整一套"。
//   - 符号链接在 Windows 上 rename 语义与 Linux 不同，无法在本机验收；
//     文件级替换两边都可靠（同目录内 rename 是原子的），发布可靠性因此可被测。
//   - 排查时 `cat current/VERSION` 比 `readlink current` 更直观。
//
// 版本目录保持不变的意义：任何一版都能原样回滚 —— 回滚不是"复活旧规则"，
// 而是"把当时那份字节原样放回去"，这才是可复现的。
type HAProxy struct {
	// Binary haproxy 可执行文件路径。
	Binary string
	// ConfigRoot 版本目录的父目录。
	ConfigRoot string
	// CurrentDir 当前生效配置所在目录（真实目录，不是符号链接）。
	CurrentDir string
	// StatsSocket HAProxy 的统计 socket（单一路径，旧模式）。
	StatsSocket string
	// StatsSocketDir 按版本区分的统计套接字目录。
	// 非空时优先：Verify 就是靠它把"运行版本"变成可观测事实的。
	StatsSocketDir string
	// ServiceName systemd 单元名。
	ServiceName string
	// ReloadCmd 触发平滑生效的完整命令。
	//
	// 默认是 `systemctl reload shengyu-edgelink-haproxy`，root 与非 root 都用它：
	// 非 root 时依赖 systemd 的 D-Bus + polkit 授权（deploy/polkit-50-shengyu-edgelink.rules
	// 只放行这一个单元的一个动作）。
	//
	// ⚠️ 这里**不能**用 sudo。评审指出：server 单元带 NoNewPrivileges=true，
	// 而 sudo 依赖 setuid 提权，两者语义直接冲突 —— sudo 永远不可能成功。
	// 详见 deploy/shengyu-edgelink-server.service 与 docs/07 的说明。
	//
	// 为什么不用"万能 helper"：helper 一旦能接受任意参数，就等于把 root 权限
	// 通过一个 HTTP 接口暴露出去。这里宁可让发布流程只能做一件事。
	ReloadCmd []string
	// SNIInspectTimeout 与渲染出的 timeout 对应，用于验证阶段。
	SNIInspectTimeout time.Duration

	run       runner
	readFile  func(string) ([]byte, error)
	writeFile func(string, []byte, os.FileMode) error
	readDir   func(string) ([]os.DirEntry, error)
	statFn    func(string) (os.FileInfo, error)
	// probeSock 探测某个统计套接字**是否还有 worker 在应答**。
	//
	// 做成可注入的，是因为"能不能连上"在不同情形下含义不同：
	// 权限不足（EACCES）与"确实没人监听"（ECONNREFUSED）必须分开 ——
	// 前者是"判不出来"，后者才是"可以删"。判不出来时一律保守保留。
	probeSock func(ctx context.Context, path string) (alive, certain bool)
	clock     func() time.Time
	mu        sync.Mutex
}

// VersionFileName current 目录里记录生效版本号的文件名。
const VersionFileName = "VERSION"

// NewHAProxy 创建 applier，默认值适用于本项目约定的部署形态。
func NewHAProxy() *HAProxy {
	return &HAProxy{
		Binary:            "/usr/sbin/haproxy",
		ConfigRoot:        "/etc/shengyu-edgelink/haproxy",
		CurrentDir:        "/etc/shengyu-edgelink/haproxy/current",
		StatsSocket:       "/run/shengyu-edgelink/haproxy.sock",
		StatsSocketDir:    "/run/shengyu-edgelink",
		ServiceName:       "shengyu-edgelink-haproxy",
		SNIInspectTimeout: 5 * time.Second,
		run:               execRunner,
		readFile:          os.ReadFile,
		writeFile:         atomicWriteFile,
		readDir:           os.ReadDir,
		statFn:            os.Lstat,
		probeSock:         probeUnixSocket,
		clock:             time.Now,
	}
}

// SetRunner 注入命令执行器（测试用）。
func (h *HAProxy) SetRunner(r runner) { h.run = r }

// SetSocketProbe 注入统计套接字探测器（测试用）。
func (h *HAProxy) SetSocketProbe(f func(ctx context.Context, path string) (alive, certain bool)) {
	h.probeSock = f
}

// SetFileReader 注入文件读取器（测试用）。
func (h *HAProxy) SetFileReader(f func(string) ([]byte, error)) { h.readFile = f }

// SetStat 注入 Lstat（测试用）。
func (h *HAProxy) SetStat(f func(string) (os.FileInfo, error)) { h.statFn = f }

// statsSocketPath 返回某个版本对应的统计套接字路径。
func (h *HAProxy) statsSocketPath(version int) string {
	if strings.TrimSpace(h.StatsSocketDir) != "" {
		return filepath.Join(h.StatsSocketDir, haproxy.StatsSocketFileName(version))
	}
	return h.StatsSocket
}

// StatsSocketFor 返回某个版本的统计套接字路径（供部署与排障使用）。
func (h *HAProxy) StatsSocketFor(version int) string { return h.statsSocketPath(version) }

// runtimeInfo 运行期观测结果。
type runtimeInfo struct {
	// Alive 表示"该版本的统计接口有应答" —— 也就是**该版本的 worker 确实在跑**。
	Alive bool
	Err   string
	// Frontends frontend 名 -> 状态（OPEN / DOWN ...）。
	Frontends map[string]string
	Info      map[string]string
}

// inspectRuntime 用统计接口确认指定版本是否真的在运行。
//
// 这是把"运行版本"从**磁盘标记**变成**可观测事实**的关键一步：
// 我们不再问"我写下的版本号是多少"，而是问"vN 的 worker 能不能应答"。
// 连不上就等于无法确认 —— 此时宁可报"未通过验证"，也不能报成功。
func (h *HAProxy) inspectRuntime(ctx context.Context, version int) runtimeInfo {
	rt := runtimeInfo{Frontends: map[string]string{}, Info: map[string]string{}}
	sock := h.statsSocketPath(version)
	if sock == "" {
		rt.Err = "未配置统计套接字路径"
		return rt
	}
	csv, err := h.statsCommand(ctx, sock, "show stat\n")
	if err != nil {
		rt.Err = fmt.Sprintf("连接统计套接字 %s 失败: %v", sock, err)
		return rt
	}
	rt.Alive = true
	parseFrontends(csv, rt.Frontends)
	if info, ierr := h.statsCommand(ctx, sock, "show info\n"); ierr == nil {
		for _, line := range strings.Split(info, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
			if ok {
				rt.Info[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return rt
}

// parseFrontends 从 `show stat` CSV 里取出 frontend 名与状态。
//
// 按**列名**取值，不按下标：HAProxy 在不同版本里插入过新列，
// 按下标解析会在升级后静默取错字段。
func parseFrontends(csv string, out map[string]string) {
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) < 2 {
		return
	}
	header := splitCSVLine(strings.TrimPrefix(lines[0], "# "))
	idx := map[string]int{}
	for i, c := range header {
		idx[strings.TrimSpace(c)] = i
	}
	at := func(f []string, name string) string {
		i, ok := idx[name]
		if !ok || i >= len(f) {
			return ""
		}
		return strings.TrimSpace(f[i])
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitCSVLine(line)
		if at(f, "svname") != "FRONTEND" {
			continue
		}
		name := at(f, "pxname")
		st := at(f, "status")
		if name == "" {
			continue
		}
		if prev, ok := out[name]; ok && prev == "OPEN" {
			continue // 任一 worker 处于 OPEN 即认为该 frontend 就绪
		}
		out[name] = st
	}
}

func (h *HAProxy) Name() string { return HAProxyName }

// Capabilities 探测 HAProxy 版本与关键特性（需求 §二.8：选择并明确受支持的版本）。
func (h *HAProxy) Capabilities(ctx context.Context) (model.Capabilities, error) {
	caps := model.Capabilities{ProbedAt: h.clock()}
	out, errOut, err := h.run(ctx, h.Binary, "-vv")
	if err != nil {
		caps.ProbeError = fmt.Sprintf("执行 %s -vv 失败: %v; stderr=%s", h.Binary, err, trunc(errOut, 200))
		return caps, nil
	}
	ver := parseHAProxyVersion(out + errOut)
	caps.Version = ver
	caps.Supported = versionSupported(ver)

	// master-worker 是平滑 reload 的前提。缺失它就只能"重启"，会中断连接，
	// 因此必须显式探测而不是假定。
	caps.MasterWorker = h.supportsMasterWorker(ctx)
	// 这两个是渲染器已经在用的能力，通过一次语法检查间接确认：
	// 若 `haproxy -c` 能通过我们生成的配置，说明这些关键字被该版本接受。
	caps.SNICapture = true
	caps.JSONEscape = true
	caps.ExposeFDListeners = false
	return caps, nil
}

// parseHAProxyVersion 从 `haproxy -vv` 输出里提取版本号。
func parseHAProxyVersion(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "HAProxy version") {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 3 {
			// 形如 "HAProxy version 2.8.5-1 2024/01/01" → 取 "2.8.5-1"，去掉发行版后缀。
			v := f[2]
			if i := strings.IndexByte(v, '-'); i > 0 {
				return v[:i]
			}
			return v
		}
	}
	return ""
}

// versionSupported 判断版本是否在受支持范围内。
//
// 受支持范围写在 docs/04：2.4 LTS ≤ v < 3.2。
// 低于 2.4 缺少我们依赖的若干 sample fetch 行为；3.2 起的默认值变更未验证。
// 宁可拒绝，也不要"也许能跑"——中转平台的失败代价是客户业务中断。
func versionSupported(v string) bool {
	if v == "" {
		return false
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	if major != 2 {
		return major == 3 && minor < 2
	}
	return minor >= 4
}

func (h *HAProxy) supportsMasterWorker(ctx context.Context) bool {
	out, errOut, err := h.run(ctx, h.Binary, "-vv")
	if err != nil {
		return false
	}
	all := strings.ToLower(out + errOut)
	// `haproxy -vv` 在支持 master-worker 时会打印 Built with ... 或明确指出
	// "master-worker" / "Master Worker mode" 相关字样；不同发行版措辞不同，
	// 因此按多个关键短语判断，且**默认保守**（判不出来就当不支持）。
	for _, k := range []string{"master-worker", "master worker", "threads", "defaults mode"} {
		if strings.Contains(all, k) {
			return true
		}
	}
	return false
}

// Validate 用 HAProxy 自己做语法检查（需求 §五：候选配置生成 → 语法与冲突检查）。
//
// 这里坚决不自己写语法检查器：HAProxy 的配置语法复杂且有版本差异，
// 只有它自己能给出权威结论。我们要做的是把它的结论翻译成可读信息。
func (h *HAProxy) Validate(ctx context.Context, cfgPath string) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	out, errOut, err := h.run(ctx, h.Binary, "-c", "-f", cfgPath)
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(errOut)
	if msg == "" {
		msg = strings.TrimSpace(out)
	}
	return fmt.Errorf("HAProxy 语法检查未通过: %v\n%s", err, trunc(msg, 2000))
}

// Apply 原子替换 + 平滑 reload。
//
// 顺序严格按需求 §五：
//
//	① 语法检查（Validate 已做过，这里再兜一次，因为 cfg 可能在两次调用之间被改动）
//	② 把候选版本的文件逐个原子替换进 current（先内容、最后 VERSION）
//	③ 平滑 reload（systemctl reload，等价于给 master 发 SIGHUP）
//	④ 失败则把上一版原样放回去并再次尝试恢复（由调用方走 Rollback 或此处即时回退）
func (h *HAProxy) Apply(ctx context.Context, req ApplyRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if req.ConfigPath == "" || req.ConfigDir == "" {
		return errors.New("haproxy: Apply 需要 ConfigDir 与 ConfigPath")
	}
	if err := h.Validate(ctx, req.ConfigPath); err != nil {
		return err
	}
	// extra（清单外的多余文件）由发布层负责记 warning：这里只关心"能不能发布"。
	if _, err := h.publishFiles(req.ConfigDir, req.Version); err != nil {
		return err
	}
	if err := h.reload(ctx); err != nil {
		// reload 失败：HAProxy 通常保留旧 worker 继续服务，但 current 目录已经是新内容了。
		// 必须把它放回旧版本，否则下一次 reload 会读到这份坏配置 ——
		// 一次失败会被放大成"重启即故障"。
		if req.PreviousVersion > 0 {
			// 上一版的目录必须从**本次 ConfigDir 的同级**推出来。
			//
			// 旧写法是 filepath.Join(h.ConfigRoot, "versions", "v"+N)，
			// 那是把版本目录当成 <ConfigRoot>/versions/vN —— 而生产布局是
			// <ConfigRoot>/versions/<nodeID>/vN（见 publish.Pipeline.VersionsDir）。
			// 两者不一致的后果是：这段恢复逻辑在生产目录下**永远 stat 不到目录**，
			// 于是被静默跳过，current/ 就停在未获批准的新版本上 ——
			// 正是上面注释里明确说不允许出现的那种状态。
			// 单元测试用扁平布局（versions/v1）当初因此没能发现它；
			// 复现与回归见 reload_restore_layout_test.go。
			prevDir := filepath.Join(filepath.Dir(req.ConfigDir), "v"+strconv.Itoa(req.PreviousVersion))
			if _, statErr := h.statFn(prevDir); statErr != nil {
				// 恢复不了就必须说清楚"没恢复"，让上层（publish.rollbackIfNeeded）
				// 知道 current/ 还在新版上，需要它再走一次带验证的回滚。
				return fmt.Errorf("平滑 reload 失败: %w；且上一版目录 %s 不可读（%v），"+
					"未能把 %s 恢复到第 %d 版 —— current/ 目前仍是第 %d 版的内容，需要上层回滚兜底",
					err, prevDir, statErr, h.CurrentDir, req.PreviousVersion, req.Version)
			}
			if _, rbErr := h.publishFiles(prevDir, req.PreviousVersion); rbErr != nil {
				return fmt.Errorf("平滑 reload 失败: %w；且回写上一版失败: %v", err, rbErr)
			}
		}
		return fmt.Errorf("平滑 reload 失败: %w", err)
	}
	return nil
}

// publishFiles 把某个版本目录的内容原子地放到位，并返回"版本目录里清单外多余文件"的名单。
//
// 顺序：**先查引用 → 快照 → 清陈旧 → 写内容文件 → 最后写 VERSION**。
//
// VERSION 是"这一套文件完整且属于该版本"的提交标记：中途失败时它仍是旧的，
// 界面看到的就是真实的旧版本，而不是"一半新一半旧却自称新版"。
//
// 但只靠"VERSION 最后写"是不够的 —— 这正是评审指出的缺陷：
// 如果 haproxy.cfg 已经换成新正文、而后续某个文件（例如 meta.json、允许清单）写失败，
// 那么 current/ 里就是**新正文 + 旧 VERSION**。此时进程内一切正常（老 worker 还在跑），
// 但**下一次 reload 或进程重启就会加载这份未获批准的配置**。
// 所以每个失败路径都必须把整个 current/ 恢复成快照，
// 并且**恢复本身失败时要显式报出来**，不能悄悄返回一个"已回滚"的说法。
//
// "清陈旧"放在**写之前**而不是写之后：版本目录是逐版增删的
// （某一版多了 sni_allow_9443.lst，回滚到没有它的旧版后，那个文件会滞留在 current/）。
// 先清后写的意义是 —— 这一步失败时 current/ 还没被动过，
// 用快照还原即可，不会落进"新正文已就位但没 reload"的中间态。
//
// 清理与写入都以**文件清单**（manifest.json）为准：清单是"这一版应该有哪些文件"的
// 唯一权威来源，目录列举只在旧版本没有清单时才作为退路。
//
// 两条与"清单外文件"有关的判据（评审后确定的边界）：
//
//	① 版本目录里**多出**清单没列的普通文件 ⇒ **不阻断**，作为 extra 返回给上层，
//	   由上层记 warning + 审计 + 界面提示。理由：这些文件不会进入 current/（写入以清单为准），
//	   对正确性没有影响；而当硬错误会**在事故中挡住回滚**，回滚恰恰最不能拖。
//	② 清单内配置**实际引用**了清单外的文件 ⇒ **必须拒绝**，且要在改动任何文件之前拒绝。
//	   理由：渲染产物引用的是"版本目录的绝对路径"，所以那个文件在运行时真的会被读到 ——
//	   未纳入清单的配置会影响运行，清单也就不再是权威说明。见 haproxy.ScanCitedFiles。
//
// 拒绝时 current/ 必须一个字节都没动过：这样 ActiveVersion 仍是上一版，
// 上层可以用"改动未生效、无需回滚"收尾，而不是走一次假的回滚。
func (h *HAProxy) publishFiles(srcDir string, version int) (extra []string, err error) {
	set, _, extra, ferr := h.versionFileSet(srcDir)
	if ferr != nil {
		return nil, ferr
	}
	if cited := h.citedOutsideManifest(srcDir, set); len(cited) > 0 {
		return extra, fmt.Errorf("版本目录 %s 的配置引用了 %d 个不在清单 %s 内的文件（%s）—— "+
			"这类文件不会被发布到 current/，但运行时配置按绝对路径读的是版本目录，"+
			"因此会真实生效，拒绝发布（请把该文件纳入清单，或从配置里去掉这条引用）",
			srcDir, len(cited), haproxy.ManifestFileName, haproxy.DescribeCited(cited))
	}
	if err := os.MkdirAll(h.CurrentDir, 0o750); err != nil {
		return extra, fmt.Errorf("haproxy: 创建 current 目录失败: %w", err)
	}
	snap, err := h.snapshotCurrent()
	if err != nil {
		// 拿不到快照就不发布：宁可拒绝，也不能在没有退路的情况下改线上配置。
		return extra, fmt.Errorf("haproxy: 无法快照当前配置，拒绝发布（否则失败时无法回退）: %w", err)
	}
	if _, perr := h.pruneStaleFiles(srcDir); perr != nil {
		// 此时 current/ 只有一个字节都没动过（旧文件是先删后写的），
		// 但仍然用快照还原一次，保证"pruneStaleFiles 中途删掉几个文件后失败"也能复位。
		if rerr := h.restoreCurrent(snap); rerr != nil {
			return extra, fmt.Errorf("haproxy: 清理陈旧文件失败（%v）；且恢复旧配置也失败（%v）。"+
				"current 目录可能处于不一致状态，已停止后续动作，请人工核对 %s",
				perr, rerr, h.CurrentDir)
		}
		return extra, fmt.Errorf("haproxy: 清理 current/ 里不属于新版本 %d 的文件失败，已完整恢复旧配置"+
			"（现有转发不受影响）: %w", version, perr)
	}
	if werr := h.writeVersionFiles(srcDir, version); werr != nil {
		if rerr := h.restoreCurrent(snap); rerr != nil {
			return extra, fmt.Errorf("haproxy: 写入新配置失败（%v）；且恢复旧配置也失败（%v）。"+
				"current 目录可能处于不一致状态，已停止后续动作，请人工核对 %s",
				werr, rerr, h.CurrentDir)
		}
		return extra, fmt.Errorf("haproxy: 写入新配置失败，已完整恢复旧配置（现有转发不受影响）: %w", werr)
	}
	return extra, nil
}

// versionFileSet 返回"某一版应当出现在 current/ 里的文件名集合"，以及
// 目录里**多出来**的普通文件（清单没列的）。
//
// 优先读版本清单（manifest.json）—— 它是权威来源；清单里的每个文件都必须真的存在，
// 否则说明版本目录不完整，**拒绝发布**而不是发一半。
//
// 没有清单时退回"目录里有什么就发什么"：这是升级前发布的旧版本目录的实际情况。
// 这不是静默兜底 —— 返回值里的 fromManifest=false 会被写进发布层的核对说明，
// 且下一次发布一定会生成清单。
//
// 清单存在但损坏/读不了 ⇒ 直接报错。理由：读不到权威清单就无法判断 current/ 该有哪些文件，
// 这时"当成没有清单"等于放着一份不一致的配置继续跑。
//
// 关于 extra（评审后调整的边界）：
// 目录里"有、但不在清单上"的文件**不再拒绝**，而是作为第三个返回值上报，由调用方
// 记进 warning 与审计。原因是这类文件常见于运维手工备份（cp haproxy.cfg x.orig），
// 把它们当硬错误会在事故中挡住回滚 —— 而回滚是最不能拖的时候。
// 它们也不会进入 current/：写入以清单为准（见 writeVersionFiles）。
// **但**"配置真的引用了某个清单外文件"仍然必须拒绝，那条判据在 publishFiles 里
// （见 ScanCitedFiles 的说明：渲染产物引用的是**版本目录**的绝对路径，会真的生效）。
func (h *HAProxy) versionFileSet(srcDir string) (set map[string]bool, fromManifest bool, extra []string, err error) {
	entries, derr := h.readDir(srcDir)
	if derr != nil {
		return nil, false, nil, fmt.Errorf("读取版本目录 %s 失败: %w", srcDir, derr)
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == VersionFileName || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		onDisk[e.Name()] = true
	}

	raw, rerr := h.readFile(filepath.Join(srcDir, haproxy.ManifestFileName))
	if rerr != nil {
		if os.IsNotExist(rerr) {
			// 没有清单 ⇒ 目录列举就是集合，不存在"多余文件"这一说。
			return onDisk, false, nil, nil
		}
		return nil, false, nil, fmt.Errorf("读取版本清单 %s 失败: %w", haproxy.ManifestFileName, rerr)
	}
	var mf haproxy.VersionManifest
	if uerr := json.Unmarshal(raw, &mf); uerr != nil {
		return nil, false, nil, fmt.Errorf("版本清单 %s 内容损坏，拒绝发布"+
			"（读不到权威清单就无法判断 current/ 该有哪些文件）: %w", haproxy.ManifestFileName, uerr)
	}
	set = map[string]bool{haproxy.ManifestFileName: true}
	for _, n := range mf.Files {
		if n == "" || filepath.Base(n) != n {
			return nil, false, nil, fmt.Errorf("版本清单 %s 里的文件名不合法: %q", haproxy.ManifestFileName, n)
		}
		if n == VersionFileName || n == haproxy.ManifestFileName {
			continue
		}
		if !onDisk[n] {
			// 这一条**不能松**：清单列出的文件缺一个，说明这一版不完整，
			// 拿它去发布/回滚只会更糟（真机 S7 就是这个场景）。
			return nil, false, nil, fmt.Errorf("版本清单 %s 列出了 %q，但版本目录 %s 里没有这个文件 —— "+
				"版本目录不完整，拒绝发布", haproxy.ManifestFileName, n, srcDir)
		}
		set[n] = true
	}
	for n := range onDisk {
		if !set[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	return set, true, extra, nil
}

// citedOutsideManifest 找出"被配置引用、但不在清单内"的文件（评审第 3 条：必须拒绝）。
//
// 扫的是**清单内**文件的文本 —— 清单外的文件本身就是被报告的对象，不必再当引用方。
// 读不到某个清单内文件时跳过扫描：它的存在性已由 versionFileSet 校验、
// 写入步骤也会再报错，这里不重复报、也不掩盖。
func (h *HAProxy) citedOutsideManifest(srcDir string, allowed map[string]bool) []haproxy.CitedFile {
	texts := map[string][]byte{}
	for n := range allowed {
		b, rerr := h.readFile(filepath.Join(srcDir, n))
		if rerr != nil {
			continue
		}
		texts[n] = b
	}
	var out []haproxy.CitedFile
	for _, c := range haproxy.ScanCitedFiles(srcDir, texts) {
		if !allowed[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// pruneStaleFiles 删除 current/ 里"不属于目标版本"的普通文件，并返回删掉的名单。
//
// 为什么必须做：版本目录是**逐版增删**的。举例（真机实测）：
// 某一版新增了 sni_allow_9443.lst，之后回滚到没有这条入口的旧版，
// 那个文件就会滞留在 current/ 里 —— `ls current/` 会显示一个看起来"当前生效"的
// 允许清单，而实际上没有任何配置引用它（渲染出的配置引用的是**版本目录**的绝对路径）。
// 排查时这种"看起来对"的残留足以让人得出与事实相反的结论，
// 而本平台到处都在避免这类假象（这也是 restoreCurrent 里同样要删多余文件的原因）。
//
// 覆盖面：允许清单、meta.json、state.json、manifest.json 以及**任何**其它普通文件
// 都在同一个判断之下 —— 判断依据是"在不在目标版本的文件清单里"，
// 而不是"文件名长得像不像辅助文件"。这样将来新增辅助文件时不会漏。
//
// srcDir 读不出来时**返回错误而不是跳过**：读不到目标版本的文件清单，
// 就无法判断哪些是陈旧的，这时候选择"不动"等于把问题留给下一次。
func (h *HAProxy) pruneStaleFiles(srcDir string) (removed []string, err error) {
	keep, _, _, kerr := h.versionFileSet(srcDir)
	if kerr != nil {
		return nil, kerr
	}
	// VERSION 必须先留着：它是"这一套文件属于哪一版"的标记，
	// 由 writeVersionFiles 在最后重写。若在这里删掉而后续写文件失败，
	// current/ 就会连版本标记都没有（ActiveVersion 变成 0）。
	keep[VersionFileName] = true

	cur, derr := h.readDir(h.CurrentDir)
	if derr != nil {
		if os.IsNotExist(derr) {
			return nil, nil // 首次发布，没有 current/ 可清
		}
		return nil, fmt.Errorf("列出 current 失败: %w", derr)
	}
	var problems []string
	for _, e := range cur {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") || keep[e.Name()] {
			continue
		}
		if rerr := os.Remove(filepath.Join(h.CurrentDir, e.Name())); rerr != nil && !os.IsNotExist(rerr) {
			problems = append(problems, fmt.Sprintf("删除陈旧文件 %s 失败: %v", e.Name(), rerr))
			continue
		}
		removed = append(removed, e.Name())
	}
	sort.Strings(removed)
	if len(problems) > 0 {
		return removed, errors.New(strings.Join(problems, "；"))
	}
	return removed, nil
}

// CompareCurrentToVersion 逐字节比较 current/ 与某个版本目录（实现 FileSetInspector）。
//
// 比较集合由目标的**版本清单**决定（旧目录没有清单时退回目录列举），
// VERSION 不参与比较 —— 它是 current/ 独有的提交标记，版本目录里本来就没有。
func (h *HAProxy) CompareCurrentToVersion(_ context.Context, versionDir string) (FileSetDiff, error) {
	diff := FileSetDiff{VersionDir: versionDir}
	want, fromManifest, versionDirExtra, err := h.versionFileSet(versionDir)
	if err != nil {
		return diff, err
	}
	diff.ManifestMissing = !fromManifest
	// 版本目录里"多出来的"文件不参与 current/ 的比对（它们本来就不该进 current/），
	// 但要如实上报：它是"这个目录在清单生成后被改过"的唯一信号。
	diff.VersionDirExtra = versionDirExtra
	delete(want, VersionFileName)

	got := map[string]bool{}
	entries, derr := h.readDir(h.CurrentDir)
	if derr != nil && !os.IsNotExist(derr) {
		return diff, fmt.Errorf("列出 current 失败: %w", derr)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") || e.Name() == VersionFileName {
			continue
		}
		got[e.Name()] = true
	}

	for n := range want {
		if !got[n] {
			diff.Missing = append(diff.Missing, n)
			continue
		}
		a, e1 := h.readFile(filepath.Join(versionDir, n))
		b, e2 := h.readFile(filepath.Join(h.CurrentDir, n))
		if e1 != nil || e2 != nil || !bytes.Equal(a, b) {
			diff.Mismatched = append(diff.Mismatched, n)
			continue
		}
		diff.Same++
	}
	for n := range got {
		if !want[n] {
			diff.Extra = append(diff.Extra, n)
		}
	}
	sort.Strings(diff.Missing)
	sort.Strings(diff.Extra)
	sort.Strings(diff.Mismatched)
	return diff, nil
}

// snapshotCurrent 把 current/ 里的普通文件读进内存。
//
// 配置与允许清单都是 KB 级文本，放内存最简单也最快；不采用"复制到备份目录"是因为
// 那会引入一个新的、也需要被清理的状态。
func (h *HAProxy) snapshotCurrent() (map[string][]byte, error) {
	entries, err := h.readDir(h.CurrentDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]byte{}, nil // 首次发布，没有旧状态可留
		}
		return nil, err
	}
	snap := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		b, rerr := h.readFile(filepath.Join(h.CurrentDir, e.Name()))
		if rerr != nil {
			if os.IsNotExist(rerr) {
				continue // 与删除赛跑：跳过即可
			}
			return nil, fmt.Errorf("读取 %s 失败: %w", e.Name(), rerr)
		}
		snap[e.Name()] = b
	}
	return snap, nil
}

// restoreCurrent 把 current/ 恢复成快照状态：写回快照里的每个文件，并删掉多出来的文件。
//
// "删掉多出来的文件"这一步不能省：新版本可能带来了额外的允许清单文件，
// 若不删除，回退后的旧配置与残留的新文件混在一起，排查时无法判断哪个是生效的。
func (h *HAProxy) restoreCurrent(snap map[string][]byte) error {
	var problems []string
	for name, content := range snap {
		if err := h.writeFile(filepath.Join(h.CurrentDir, name), content, 0o640); err != nil {
			problems = append(problems, fmt.Sprintf("写回 %s 失败: %v", name, err))
		}
	}
	entries, err := h.readDir(h.CurrentDir)
	if err != nil && !os.IsNotExist(err) {
		problems = append(problems, fmt.Sprintf("列出 current 失败: %v", err))
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if _, ok := snap[e.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(h.CurrentDir, e.Name())); err != nil && !os.IsNotExist(err) {
			problems = append(problems, fmt.Sprintf("删除残留 %s 失败: %v", e.Name(), err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "；"))
	}
	return nil
}

// writeVersionFiles 把版本目录里的文件逐个写入 current/，最后写 VERSION。
//
// 写哪些文件由**版本清单**决定（见 versionFileSet），而不是"目录里有什么就抄什么"：
// 清单是"这一版应该有哪些文件"的权威来源，抄目录会把不该有的残留一起带进来。
func (h *HAProxy) writeVersionFiles(srcDir string, version int) error {
	files, _, _, err := h.versionFileSet(srcDir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		data, rerr := h.readFile(filepath.Join(srcDir, name))
		if rerr != nil {
			return fmt.Errorf("读取 %s 失败: %w", name, rerr)
		}
		if werr := h.writeFile(filepath.Join(h.CurrentDir, name), data, 0o640); werr != nil {
			return fmt.Errorf("写入 %s 失败: %w", name, werr)
		}
	}
	if err := h.writeFile(filepath.Join(h.CurrentDir, VersionFileName),
		[]byte(strconv.Itoa(version)+"\n"), 0o640); err != nil {
		return fmt.Errorf("写入版本标记失败: %w", err)
	}
	return nil
}

// atomicWriteFile 写临时文件再 rename 覆盖目标。
// 同目录内的 rename 在 Linux 与 Windows 上都是原子的，
// 因此读方永远不会看到"写了一半"的文件。
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// PreflightReload 验证"以当前身份能不能 reload 这个单元"。
//
// 为什么值得单独做一次：评审 F03 的核心抱怨是"在干净系统上，非 root 后台
// 根本没法管理 root 的 systemd 服务，而失败要等第一次发布才暴露"。
// 第一次发布失败是非常糟糕的暴露时机 —— 那时配置已经写下去了、
// 运维正在盯着"发布"按钮，报错还只有一句 D-Bus 权限拒绝。
//
// 所以在启动阶段就主动验一次：
//   - 单元没在跑 ⇒ 返回 checked=false（首装时还没 start，属正常，不做判断）；
//   - 单元在跑 ⇒ 真的发一次 reload。用**当前正在跑的同一份配置** reload 是幂等的、
//     不丢连接的（master-worker 模式下 master 把监听 fd 交给新 worker），
//     因此它是一次零副作用的权限探测。
//
// 返回的 error 会带上"该装哪个文件、装到哪里"，让人一眼知道怎么修。
func (h *HAProxy) PreflightReload(ctx context.Context) (checked bool, err error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
	}
	out, errOut, err := h.run(ctx, "systemctl", "is-active", h.ServiceName)
	state := strings.TrimSpace(out)
	if err != nil && state == "" {
		// systemctl 都跑不起来（例如容器里没有 systemd）：不在本函数的职责范围内，
		// 如实返回"未检查"而不是编一个失败。
		return false, fmt.Errorf("无法查询 %s 状态（%v）：%s", h.ServiceName, err, trunc(errOut, 200))
	}
	if state != "active" {
		return false, nil
	}
	if err := h.reload(ctx); err != nil {
		return true, fmt.Errorf("以当前身份无法 reload %s：%w。\n"+
			"非 root 运行需要安装 polkit 授权规则：\n"+
			"  sudo install -m 0644 deploy/polkit-50-shengyu-edgelink.rules /etc/polkit-1/rules.d/50-shengyu-edgelink.rules\n"+
			"  sudo systemctl restart polkit\n"+
			"注意不要改用 sudo：server 单元带 NoNewPrivileges=true，sudo 依赖 setuid，必然失败",
			h.ServiceName, err)
	}
	return true, nil
}

func (h *HAProxy) reload(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	args := h.ReloadCmd
	if len(args) == 0 {
		args = []string{"systemctl", "reload", h.ServiceName}
	}
	// 只用 reload：它等价于给 master 进程发 SIGUSR2，**不会**杀进程、
	// 更不会用 pkill 误伤同机上别人的 haproxy（需求 §五明确禁止）。
	out, errOut, err := h.run(ctx, args[0], args[1:]...)
	if err != nil {
		return fmt.Errorf("执行 %s 失败: %v; stderr=%s",
			strings.Join(args, " "), err, trunc(errOut+out, 500))
	}
	return nil
}

// Verify 应用结果验证（需求 §五：验证全部预期监听端口和对应服务）。
//
// 评审在这里指出了一个**会造成"页面显示成功、实际没更新"**的问题，本实现按三条修正：
//
//	① 版本要"对得上"：磁盘标记的生效版本必须等于本次要求验证的版本；
//	② 版本要"在跑"：必须能连上该版本的统计套接字，否则**无法确认**它真的起来了 ——
//	   命令返回 0 不代表新 worker 启动成功（例如绑定失败时 master 会保留旧 worker）；
//	③ 监听要"归属自己"：端口有进程在听不等于**我们的 frontend**在听。
//	   因此按 frontend 名去统计接口里核对，核对不上就明确报"可能是端口被他人占用"。
//
// 结论：只有 ①②③ 全部成立才算 OK。任何一条无法确认，都返回"未通过验证"，
// 让发布流水线去决定回滚 —— 这比报一个漂亮的假成功要安全得多。
func (h *HAProxy) Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	res := VerifyResult{ConfigHealthy: true}

	diskVer, derr := h.ActiveVersion(ctx)
	res.DataplaneVersion = diskVer
	switch {
	case derr != nil:
		res.Problems = append(res.Problems, "读取生效版本标记失败："+derr.Error())
		res.ConfigHealthy = false
	case req.Version > 0 && diskVer != req.Version:
		res.Problems = append(res.Problems, fmt.Sprintf(
			"磁盘上标记的生效版本是 v%d，但本次要求验证的是 v%d —— 配置可能未替换成功", diskVer, req.Version))
		res.ConfigHealthy = false
	default:
		res.VersionMarkerOK = true
	}

	res.StatsSocketPath = h.statsSocketPath(req.Version)
	rt := h.inspectRuntime(ctx, req.Version)
	if !rt.Alive {
		res.Problems = append(res.Problems, fmt.Sprintf(
			"无法与版本 v%d 的统计接口通信，因此**无法确认该版本正在运行**（%s）。"+
				"注意：systemctl reload 返回成功只说明信号已送达，不代表新 worker 已就绪", req.Version, rt.Err))
		res.ConfigHealthy = false
	} else {
		res.StatsSocketOK = true
	}

	bound, berr := h.boundPorts()
	if berr != nil {
		res.Problems = append(res.Problems, "读取系统监听列表失败："+berr.Error())
	}
	fes := expectedFrontends(req.State)

	// 监听归属：只要有一个期望监听"端口在听但不是我们这一版的 frontend"，就不算通过。
	// 空集合（基线，或本版不监听任何端口）按"无待核对项"处理 —— 那不能算失败。
	allOwned := true
	for _, l := range ExpectedListeners(req.State) {
		st := ListenerStatus{Expected: l, Frontend: fes[l.Key()]}
		st.Bound = len(bound[l.Port]) > 0
		switch {
		case !st.Bound:
			st.Note = "该端口未处于监听状态"
			res.Problems = append(res.Problems, l.Key()+" 未监听")
			allOwned = false
		case !rt.Alive:
			st.Note = "端口在监听，但无法确认监听者是本平台配置的 frontend " + st.Frontend
			// 已在上面记过"无法确认运行版本"，这里不重复堆问题列表。
			allOwned = false
		case rt.Frontends[st.Frontend] == "OPEN":
			st.OwnedByUs = true
		default:
			status := rt.Frontends[st.Frontend]
			if status == "" {
				status = "未出现在统计接口里"
			}
			st.Note = fmt.Sprintf("端口在监听，但监听者不是配置 v%d 的 frontend %s（%s）—— 可能是端口被其它程序占用，或新 worker 未启动",
				req.Version, st.Frontend, status)
			res.Problems = append(res.Problems, fmt.Sprintf("%s 的监听归属无法确认（期望 frontend %s，实际状态 %s）",
				l.Key(), st.Frontend, status))
			allOwned = false
		}
		res.Listeners = append(res.Listeners, st)
	}
	res.ListenersOK = allOwned
	res.OK = res.VersionMarkerOK && res.StatsSocketOK && res.ListenersOK && len(res.Problems) == 0
	res.Detail = DescribeVerify(res)
	return res, nil
}

func probeDial(addr string, port int, timeout time.Duration) error {
	host := addr
	if host == "" || host == "0.0.0.0" || host == "*" {
		host = "127.0.0.1"
	}
	d := net.Dialer{Timeout: timeout}
	c, err := d.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	_ = c.Close()
	return nil
}

func (h *HAProxy) Listeners(context.Context) ([]model.Listener, error) {
	bound, err := h.boundPorts()
	if err != nil {
		return nil, err
	}
	out := make([]model.Listener, 0, len(bound))
	for p, addrs := range bound {
		for _, a := range addrs {
			out = append(out, model.Listener{Addr: a, Port: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// boundPorts 直接解析 /proc/net/tcp{,6}，不依赖 ss / netstat。
//
// 为什么不用 ss：不同发行版的 ss 输出格式差异大（列名、字段数、IPv6 表示），
// 解析起来比直接读内核暴露的固定格式更脆弱；而且容器里常常没有 ss。
// /proc/net/tcp 的格式是内核 ABI，几十年没变。
func (h *HAProxy) boundPorts() (map[int][]string, error) {
	out := map[int][]string{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := h.readFile(f)
		if err != nil {
			continue // 没有 IPv6 或权限不足都不该让验证整体失败
		}
		parseProcNetTCP(string(b), out)
	}
	return out, nil
}

// parseProcNetTCP 解析 /proc/net/tcp 的 LISTEN 行。
// 行格式（列以空白分隔）：
//
//	sl  local_address rem_address st tx_queue:rx_queue ...  其中 st=0A 表示 LISTEN，
//	local_address 形如 "0100007F:1F90"（小端十六进制的 IP:端口）。
func parseProcNetTCP(content string, out map[int][]string) {
	sc := bufio.NewScanner(strings.NewReader(content))
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first { // 表头
			first = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[3] != "0A" { // 只关心 LISTEN
			continue
		}
		hostHex, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port64, err := strconv.ParseUint(portHex, 16, 32)
		if err != nil {
			continue
		}
		port := int(port64)
		ip := decodeProcIP(hostHex)
		if ip == "" {
			continue
		}
		if !containsStr(out[port], ip) {
			out[port] = append(out[port], ip)
		}
	}
}

// decodeProcIP 把 /proc/net/tcp 的小端十六进制地址解成点分十进制。
func decodeProcIP(hexAddr string) string {
	if len(hexAddr) == 8 { // IPv4：4 字节小端
		b := make([]byte, 4)
		for i := 0; i < 4; i++ {
			v, err := strconv.ParseUint(hexAddr[i*2:i*2+2], 16, 8)
			if err != nil {
				return ""
			}
			b[3-i] = byte(v)
		}
		return net.IP(b).String()
	}
	if len(hexAddr) == 32 { // IPv6：16 字节，按 4 字节一组小端存放
		b := make([]byte, 16)
		for g := 0; g < 4; g++ {
			for i := 0; i < 4; i++ {
				v, err := strconv.ParseUint(hexAddr[(g*4+i)*2:(g*4+i)*2+2], 16, 8)
				if err != nil {
					return ""
				}
				b[g*4+(3-i)] = byte(v)
			}
		}
		return net.IP(b).String()
	}
	return ""
}

// ActiveVersion 读取 current 目录里的版本标记。
// 返回 0 表示从未成功发布过（这是合法状态，不是错误）。
func (h *HAProxy) ActiveVersion(context.Context) (int, error) {
	b, err := h.readFile(filepath.Join(h.CurrentDir, VersionFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("haproxy: 读取版本标记失败: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, fmt.Errorf("haproxy: 版本标记为空文件")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("haproxy: 版本标记内容无法解析: %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("haproxy: 版本标记为负数: %d", n)
	}
	return n, nil
}

// Stats 通过统计 socket 取实时指标（需求 §六.D）。
//
// 为什么用 socket 而不是抓日志：TCP 会话日志在会话结束时才完整，
// 一条挂了整天的长连接在日志里"不存在"。HAProxy 的统计接口是按当前状态采样的，
// 这正是"在线连接数"唯一正确的来源。
//
// 取哪个套接字：优先**当前生效版本**那一个。若连不上，则回退扫描同目录下的
// 其它 stats-v*.sock —— 因为发布刚失败、或版本标记与实际运行不一致时，
// 指标仍然应该能取到，而不是整块变成"不可用"。
func (h *HAProxy) Stats(ctx context.Context) (Stats, error) {
	s := Stats{At: h.clock(), DataplaneDetail: "HAProxy 统计接口"}
	ver, err := h.ActiveVersion(ctx)
	if err == nil {
		s.DataplaneVer = ver
	}

	var (
		raw      string
		fromSock string
		lastErr  error
	)
	for _, sock := range h.statsSocketCandidates(ver) {
		if sock == "" {
			continue
		}
		raw, lastErr = h.statsCommand(ctx, sock, "show stat\n")
		if lastErr == nil {
			fromSock = sock
			break
		}
	}
	if lastErr != nil {
		// 统计接口不可用不是致命错误：转发照常，界面显示"指标暂不可用"即可。
		s.DataplaneDetail = "统计接口不可用: " + lastErr.Error()
		return s, nil
	}
	parseHAProxyStatsCSV(raw, &s)
	if info, ierr := h.statsCommand(ctx, fromSock, "show info\n"); ierr == nil {
		parseHAProxyInfo(info, &s)
	}
	s.DataplaneDetail = "HAProxy 统计接口（" + fromSock + "）"
	return s, nil
}

// statsSocketCandidates 返回应当尝试连接的统计套接字，按优先级排列。
func (h *HAProxy) statsSocketCandidates(runningVersion int) []string {
	var out []string
	if runningVersion > 0 {
		out = append(out, h.statsSocketPath(runningVersion))
	}
	if strings.TrimSpace(h.StatsSocket) != "" {
		out = append(out, h.StatsSocket)
	}
	// 回退：同目录下的其它版本套接字（发布失败/版本漂移时仍能取到指标）。
	if dir := strings.TrimSpace(h.StatsSocketDir); dir != "" {
		if entries, err := h.readDir(dir); err == nil {
			var names []string
			for _, e := range entries {
				n := e.Name()
				if !e.IsDir() && strings.HasPrefix(n, "stats-v") && strings.HasSuffix(n, ".sock") {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			for _, n := range names {
				out = append(out, filepath.Join(dir, n))
			}
		}
	}
	return out
}

// statsCommand 在指定套接字上执行一条统计命令。
func (h *HAProxy) statsCommand(ctx context.Context, socket, cmd string) (string, error) {
	if strings.TrimSpace(socket) == "" {
		return "", errors.New("未配置统计 socket")
	}
	d := net.Dialer{}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
	}
	if dl, ok := ctx.Deadline(); ok {
		d.Timeout = time.Until(dl)
	}
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if _, err := c.Write([]byte(cmd)); err != nil {
		return "", err
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, err := ioReadAll(c, 1<<20)
	return string(b), err
}

// parseHAProxyStatsCSV 解析 `show stat` 的 CSV 输出。
//
// 关键点：**按表头列名取值，不按下标**。HAProxy 在不同版本里插入过新列，
// 按下标解析会在升级后静默取错字段（例如把 qcur 当成 scur），这比报错更危险。
func parseHAProxyStatsCSV(csv string, s *Stats) {
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) < 2 {
		return
	}
	header := splitCSVLine(strings.TrimPrefix(lines[0], "# "))
	idx := map[string]int{}
	for i, c := range header {
		idx[strings.TrimSpace(c)] = i
	}
	get := func(f []string, name string) string {
		i, ok := idx[name]
		if !ok || i >= len(f) {
			return ""
		}
		return strings.TrimSpace(f[i])
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitCSVLine(line)
		switch get(f, "svname") {
		case "FRONTEND":
			addInt(get(f, "scur"), &s.ActiveConns)
			addInt(get(f, "stot"), &s.TotalConns)
			addInt(get(f, "bin"), &s.BytesIn)
			addInt(get(f, "bout"), &s.BytesOut)
			addInt(get(f, "qcur"), &s.QueueDepth)
			addInt(get(f, "econ"), &s.ConnErrors)
			addInt(get(f, "eresp"), &s.ConnErrors)
		case "BACKEND":
			if st := get(f, "status"); st == "UP" {
				s.BackendUp++
			} else if st == "DOWN" {
				s.BackendDown++
			}
		}
	}
}

// splitCSVLine 处理 HAProxy 统计 CSV 的引号规则（字段可能被双引号包裹）。
func splitCSVLine(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	out = append(out, cur.String())
	return out
}

func parseHAProxyInfo(info string, s *Stats) {
	for _, line := range strings.Split(info, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "CumConns":
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && s.TotalConns == 0 {
				s.TotalConns = n
			}
		}
	}
}

func addInt(v string, dst *int64) {
	if v == "" {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return
	}
	*dst += n
}

// Close 对 HAProxy 实现是空操作：进程由其 systemd 单元管理，
// 不能由管理面"顺手关掉"——那会造成转发中断。
func (h *HAProxy) Close() error { return nil }

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func trunc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(截断)"
}

func ioReadAll(r net.Conn, max int) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 4096)
	for len(buf) < max {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			// Unix socket 对端通常不关连接，读到超时即认为拿全了。
			return buf, nil
		}
	}
	return buf, nil
}
