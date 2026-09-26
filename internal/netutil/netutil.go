// Package netutil 提供系统级端口占用探测。
//
// 为什么单独成包：创建数据面入口、发布前冲突检查都需要在「写文件 + reload」之前
// 知道某个地址:端口是否已经被系统里的进程占用（例如另一程序已经占了 443）。
// 这套逻辑与具体数据面实现无关，抽出来既能被 api 包复用，也不引入循环依赖。
//
// 实现只依赖 /proc/net/tcp 与 /proc/net/tcp6（Linux）。非 Linux 环境下
// ListeningPorts 返回空结果且不报错——调用方据此「无法判断即放行」，
// 把最终裁决交给发布后的 Verify + 自动回滚，避免在没 /proc 的环境里误伤。
package netutil

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ListenEntry 一条处于 LISTEN 状态的 socket。
//
// Inode 是反查"到底哪个进程占着"的钥匙：/proc/net/tcp 只给到 socket inode，
// 要落到进程名必须再拿它去 /proc/<pid>/fd 里比对。
type ListenEntry struct {
	Addr  string
	Port  int
	Inode uint64
}

// ListeningPorts 返回当前系统处于 LISTEN 状态的 端口 -> 监听地址列表。
//
// 地址是点分十进制（IPv4）或标准 IPv6 文本形式。仅 Linux 有数据；其它平台返回空 map。
func ListeningPorts() (map[int][]string, error) {
	es, err := ListenEntries()
	if err != nil {
		return nil, err
	}
	out := map[int][]string{}
	for _, e := range es {
		if !containsStr(out[e.Port], e.Addr) {
			out[e.Port] = append(out[e.Port], e.Addr)
		}
	}
	return out, nil
}

// ListenEntries 返回带 socket inode 的监听条目。
func ListenEntries() ([]ListenEntry, error) {
	var out []ListenEntry
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			// 没有 IPv6 或权限不足都不该让检查整体失败：当作「没查到」继续。
			continue
		}
		parseProcNetTCP(string(b), &out)
	}
	return out, nil
}

// parseProcNetTCP 解析 /proc/net/tcp 的 LISTEN 行。
//
// 行格式（列以空白分隔）：
//
//	sl  local_address rem_address st tx_queue:rx_queue ... uid timeout inode
//
// 其中 st=0A 表示 LISTEN，local_address 形如 "0100007F:1F90"（小端十六进制的 IP:端口），
// inode 在第 10 列。
// 逻辑与 dataplane 包的 parseProcNetTCP 保持一致，方便单测复用同一套解析。
func parseProcNetTCP(content string, out *[]ListenEntry) {
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
		ip := decodeProcIP(hostHex)
		if ip == "" {
			continue
		}
		e := ListenEntry{Addr: ip, Port: int(port64)}
		if len(fields) > 9 {
			if ino, ierr := strconv.ParseUint(fields[9], 10, 64); ierr == nil {
				e.Inode = ino
			}
		}
		*out = append(*out, e)
	}
}

// decodeProcIP 把 /proc/net/tcp 的小端十六进制地址解成点分十进制或 IPv6 文本。
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

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// normalizeAddr 把绑定地址规范化成点分十进制 / IPv6 文本 / 通配标记。
func normalizeAddr(a string) string {
	switch a {
	case "", "*", "0.0.0.0", "::":
		return "0.0.0.0"
	}
	return a
}

// PortInUse 判断 addr:port 在给定的监听表中是否被占用（LISTEN）。
//
// addr 为通配（空 / 0.0.0.0 / :: / *）时，只要该端口有任何监听即算占用；
// addr 为具体地址时，被自身精确占用、或被某个通配监听（0.0.0.0/::）覆盖也算占用。
// 返回 (是否占用, 占用者地址列表)。
func PortInUse(addr string, port int, ports map[int][]string) (bool, []string) {
	addrs, ok := ports[port]
	if !ok {
		return false, nil
	}
	norm := normalizeAddr(addr)
	if norm == "0.0.0.0" {
		// 想绑通配：任何监听都冲突。
		return true, addrs
	}
	// 具体地址：自己精确占用，或被通配占用。
	if containsStr(addrs, norm) {
		return true, []string{norm}
	}
	for _, a := range addrs {
		if normalizeAddr(a) == "0.0.0.0" {
			return true, []string{a + " (通配)"}
		}
	}
	return false, nil
}

// Owner 一个端口占用者。
type Owner struct {
	// Addr 该 socket 监听的地址（"::" 之类的通配意味着它同时占据了 IPv4）。
	Addr string `json:"addr"`
	// PID 进程号；0 表示**没能解析出来**（见 resolveSocketOwners 的权限说明）。
	PID int `json:"pid"`
	// Comm 进程名（如 "xray"）；解析不到时为空字符串。
	Comm string `json:"comm,omitempty"`
}

// Conflict 一次端口冲突的完整描述，界面据此显示"被谁占了"。
type Conflict struct {
	Addr   string  `json:"addr"`
	Port   int     `json:"port"`
	Owners []Owner `json:"owners"`
	// ProcessResolved 是否成功解析出进程名。
	//
	// 为 false 时界面不能显示"无人占用"——只能说"有程序占用但查不出是谁"，
	// 并给出 InspectCmd 让运维自己确认。
	ProcessResolved bool `json:"process_resolved"`
	// InspectCmd 解析不出进程时给运维的可复制命令。
	InspectCmd string `json:"inspect_cmd,omitempty"`
}

// Describe 把冲突讲成一句话（日志与 API 报错复用，保证口径一致）。
func (c *Conflict) Describe() string {
	if c == nil {
		return ""
	}
	parts := make([]string, 0, len(c.Owners))
	for _, o := range c.Owners {
		if o.PID > 0 && o.Comm != "" {
			parts = append(parts, fmt.Sprintf("%s (pid %d) 监听 %s", o.Comm, o.PID, o.Addr))
		} else if o.PID > 0 {
			parts = append(parts, fmt.Sprintf("pid %d 监听 %s", o.PID, o.Addr))
		} else {
			parts = append(parts, fmt.Sprintf("某进程监听 %s（进程名未解析出来）", o.Addr))
		}
	}
	s := fmt.Sprintf("%s:%d 已被占用：%s", c.Addr, c.Port, strings.Join(parts, "；"))
	if !c.ProcessResolved {
		s += fmt.Sprintf("。本服务以非 root 运行，读不到其它用户进程的 fd，请在服务器上执行 %s 确认", c.InspectCmd)
	}
	return s
}

// InspectPort 检查 addr:port 是否被占用，并尽力查清占用者进程。
//
// 返回 (冲突详情, 是否被占用, 错误)。未被占用时冲突详情为 nil。
func InspectPort(addr string, port int) (*Conflict, bool, error) {
	es, err := ListenEntries()
	if err != nil {
		return nil, false, err
	}
	norm := normalizeAddr(addr)
	var (
		mine   []ListenEntry
		inodes []uint64
	)
	for _, e := range es {
		if e.Port != port {
			continue
		}
		// 想绑通配 → 任何监听都冲突；想绑具体地址 → 精确命中或被通配覆盖都算冲突。
		if norm == "0.0.0.0" || normalizeAddr(e.Addr) == "0.0.0.0" || e.Addr == norm {
			mine = append(mine, e)
			inodes = append(inodes, e.Inode)
		}
	}
	if len(mine) == 0 {
		return nil, false, nil
	}
	c := &Conflict{Addr: addr, Port: port}
	byInode := resolveSocketOwners(inodes)
	for _, e := range mine {
		o := Owner{Addr: e.Addr}
		if p, ok := byInode[e.Inode]; ok {
			o.PID, o.Comm = p.PID, p.Comm
			c.ProcessResolved = true
		}
		c.Owners = append(c.Owners, o)
	}
	if !c.ProcessResolved {
		c.InspectCmd = fmt.Sprintf("ss -ltnp | grep ':%d'", port)
	}
	return c, true, nil
}

// resolveSocketOwners 把 socket inode 反查成进程（尽力而为）。
//
// 关键限制：本服务以非 root 的 shengyu 身份运行，读不到**属于其它用户的**进程目录
// （/proc/<pid>/fd 需要权限）。所以这是"能查到就查，查不到如实说查不到"。
//
// 绝不能因为解析不出进程名就判定"端口空闲" —— 那会把真实冲突掩盖成可用，
// 比报错更危险。调用方只依据"监听表里有没有这一条"来判断占用。
func resolveSocketOwners(inodes []uint64) map[uint64]Owner {
	out := map[uint64]Owner{}
	want := make(map[uint64]bool, len(inodes))
	for _, i := range inodes {
		if i != 0 {
			want[i] = true
		}
	}
	if len(want) == 0 {
		return out
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, p := range procs {
		if !p.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue // 非数字目录（如 self、sys）不是进程
		}
		fdDir := filepath.Join("/proc", p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // 权限不足或进程已退出：正常现象，跳过
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			ino, ok := socketInode(target)
			if !ok || !want[ino] {
				continue
			}
			comm := ""
			if b, rerr := os.ReadFile(filepath.Join("/proc", p.Name(), "comm")); rerr == nil {
				comm = strings.TrimSpace(string(b))
			}
			out[ino] = Owner{PID: pid, Comm: comm}
			break // 一个进程只需记一次
		}
	}
	return out
}

// socketInode 从 "socket:[12345]" 里取出 inode。
func socketInode(target string) (uint64, bool) {
	const prefix, suffix = "socket:[", "]"
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, suffix) {
		return 0, false
	}
	v, err := strconv.ParseUint(target[len(prefix):len(target)-len(suffix)], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// IsPortInUse 是对 PortInUse 的便捷封装：直接读系统监听表判断 addr:port 是否被占用。
//
// 非 Linux 或 /proc 不可读时返回 (false, nil) —— 即「无法判断即放行」，
// 把最终裁决交给发布后的 Verify + 自动回滚，避免在没 /proc 的环境里误伤。
func IsPortInUse(addr string, port int) (bool, []string, error) {
	ports, err := ListeningPorts()
	if err != nil {
		return false, nil, err
	}
	inUse, owners := PortInUse(addr, port, ports)
	return inUse, owners, nil
}
