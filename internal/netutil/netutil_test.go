package netutil

import (
	"strings"
	"testing"
)

// portsOf 把解析结果还原成 "端口 -> 地址列表"，方便沿用原有的表驱动断言。
func portsOf(es []ListenEntry) map[int][]string {
	m := map[int][]string{}
	for _, e := range es {
		found := false
		for _, a := range m[e.Port] {
			if a == e.Addr {
				found = true
			}
		}
		if !found {
			m[e.Port] = append(m[e.Port], e.Addr)
		}
	}
	return m
}

func TestDecodeProcIP(t *testing.T) {
	cases := []struct {
		hex  string
		want string
	}{
		{"0100007F", "127.0.0.1"}, // 127.0.0.1 小端
		{"00000000", "0.0.0.0"},   // 通配 IPv4
		{"808A2201", "1.34.138.128"},
	}
	for _, c := range cases {
		if got := decodeProcIP(c.hex); got != c.want {
			t.Errorf("decodeProcIP(%q) = %q, want %q", c.hex, got, c.want)
		}
	}
}

func TestParseProcNetTCP(t *testing.T) {
	// 模拟 /proc/net/tcp：表头 + 两行 LISTEN(0A) + 一行非 LISTEN(01=TIME_WAIT)。
	content := "" +
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when\n" +
		"   0: 0100007F:1F91 00000000:0000 0A 00000000:00000000 01\n" + // 127.0.0.1:8081 LISTEN（0x1F91 = 8081）
		"   1: 00000000:01BB 00000000:0000 0A 00000000:00000000 01\n" + // 0.0.0.0:443 LISTEN
		"   2: 808A2201:0050 00000000:0000 01 00000000:00000000 01\n" // 1.34.138.128:80 TIME_WAIT（忽略）

	var es []ListenEntry
	parseProcNetTCP(content, &es)
	out := portsOf(es)

	if got := out[8081]; len(got) != 1 || got[0] != "127.0.0.1" {
		t.Errorf("port 8081 listeners = %v, want [127.0.0.1]", got)
	}
	if got := out[443]; len(got) != 1 || got[0] != "0.0.0.0" {
		t.Errorf("port 443 listeners = %v, want [0.0.0.0]", got)
	}
	if _, ok := out[80]; ok {
		t.Errorf("port 80 (TIME_WAIT) should be absent, got %v", out[80])
	}
}

func TestPortInUse(t *testing.T) {
	ports := map[int][]string{
		443:  {"0.0.0.0"},
		8081: {"127.0.0.1"},
		8443: {"192.168.1.10", "0.0.0.0"},
	}
	cases := []struct {
		addr   string
		port   int
		want   bool
		owners []string
	}{
		{"0.0.0.0", 443, true, []string{"0.0.0.0"}},      // 通配绑 443，被通配占用
		{"127.0.0.1", 443, true, []string{"0.0.0.0"}},    // 具体绑 443，被通配覆盖
		{"0.0.0.0", 8081, true, []string{"127.0.0.1"}},   // 通配绑 8081，被具体占用
		{"127.0.0.1", 8081, true, []string{"127.0.0.1"}}, // 精确占用
		{"0.0.0.0", 8443, true, []string{"192.168.1.10", "0.0.0.0"}},
		{"10.0.0.5", 9443, false, nil}, // 空闲端口
		{"0.0.0.0", 9443, false, nil},
	}
	for _, c := range cases {
		got, owners := PortInUse(c.addr, c.port, ports)
		if got != c.want {
			t.Errorf("PortInUse(%q,%d) = %v, want %v", c.addr, c.port, got, c.want)
		}
		if c.want && len(owners) == 0 {
			t.Errorf("PortInUse(%q,%d) reported in-use but no owners", c.addr, c.port)
		}
	}
}

// TestPortInUseIPv6WildcardCoversIPv4 锁死一个在真机上踩到的关键场景。
//
// 现象：某台服务器上的 xray 把 443 绑在**IPv6 通配 [::]** 上（Linux 默认双栈，
// 该 socket 同时接受 IPv4 连接），于是：
//   - `ss -ltnp` 显示 `*:443`；
//   - 但 `/proc/net/tcp`（只有 IPv4）里**查不到 443**，只在 `/proc/net/tcp6` 里有。
//
// 后果：任何"只扫 /proc/net/tcp"的实现都会**漏判**这个冲突，
// 让用户在已被占用的 443 上建入口，直到发布时 HAProxy 绑定失败才发现。
// 因此必须同时读 tcp6，并把 "::" 当作通配（覆盖所有 IPv4 地址）。
func TestPortInUseIPv6WildcardCoversIPv4(t *testing.T) {
	// 模拟只有 tcp6 报出来的监听：443 被 IPv6 通配占用。
	ports := map[int][]string{443: {"::"}}

	// 想绑 IPv4 通配 → 冲突
	if got, owners := PortInUse("0.0.0.0", 443, ports); !got || len(owners) == 0 {
		t.Errorf("PortInUse(0.0.0.0,443) = (%v,%v), want 占用且有占用者", got, owners)
	}
	// 想绑具体 IPv4 → 被 IPv6 通配覆盖，同样冲突
	if got, _ := PortInUse("1.2.3.4", 443, ports); !got {
		t.Errorf("PortInUse(1.2.3.4,443) = false, want true（IPv6 通配应覆盖 IPv4）")
	}
	// 其它端口不受影响
	if got, _ := PortInUse("0.0.0.0", 8443, ports); got {
		t.Errorf("PortInUse(0.0.0.0,8443) = true, want false")
	}
}

// TestParseProcNetTCP6Wildcard 证明 tcp6 的 32 位十六进制通配地址能被解成 "::"。
// 这是"读真 /proc"那一环：解不出来，上一个测试里的 "::" 就永远出现不了。
func TestParseProcNetTCP6Wildcard(t *testing.T) {
	content := "" +
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when\n" +
		"   0: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000 01\n"
	var es []ListenEntry
	parseProcNetTCP(content, &es)
	out := portsOf(es)
	if got := out[443]; len(got) != 1 || got[0] != "::" {
		t.Errorf("port 443 listeners = %v, want [::]", got)
	}
}

// TestParseProcNetTCPInode 钉住"能读出 socket inode"这一环。
//
// 没有 inode 就无法反查占用进程，界面只能说"端口被占用"却说不出被谁占 ——
// 在 443 常年被别的程序占用的机器上，这等于让运维自己挨个排查。
func TestParseProcNetTCPInode(t *testing.T) {
	content := "" +
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when\n" +
		// 列序按内核 seq_printf 的真实格式：
		// sl local rem st tx:rx tr:tm->when retrnsmt uid timeout inode
		"   0: 00000000:01BB 00000000:0000 0A 00000000:00000000 02:00000000 00000000     0        0 424242 1 ffff8801\n"
	var es []ListenEntry
	parseProcNetTCP(content, &es)
	if len(es) != 1 {
		t.Fatalf("应解析出 1 条，实际 %d 条", len(es))
	}
	if es[0].Inode != 424242 {
		t.Fatalf("inode = %d, want 424242", es[0].Inode)
	}
}

// TestSocketInode 钉住 "socket:[N]" 的解析（这是反查进程的入口）。
func TestSocketInode(t *testing.T) {
	if v, ok := socketInode("socket:[424242]"); !ok || v != 424242 {
		t.Fatalf("socketInode = (%d,%v), want (424242,true)", v, ok)
	}
	if _, ok := socketInode("pipe:[123]"); ok {
		t.Fatal("非 socket 的目标不应被解析成 inode")
	}
	if _, ok := socketInode("socket:[abc]"); ok {
		t.Fatal("非法数字的 inode 应被拒绝")
	}
}

// TestConflictDescribesOwner 钉住"冲突要能说清是谁、查不出时不能说成没人占"。
func TestConflictDescribesOwner(t *testing.T) {
	c := &Conflict{
		Addr:            "0.0.0.0",
		Port:            443,
		Owners:          []Owner{{Addr: "::", PID: 615, Comm: "xray"}},
		ProcessResolved: true,
	}
	got := c.Describe()
	for _, want := range []string{"443", "xray", "615"} {
		if !strings.Contains(got, want) {
			t.Fatalf("冲突描述 %q 应包含 %q", got, want)
		}
	}
	// 解析不出进程时：必须给出排查命令，且绝不能说"无人占用"
	c2 := &Conflict{Addr: "0.0.0.0", Port: 443, Owners: []Owner{{Addr: "::"}}, InspectCmd: "ss -ltnp | grep ':443'"}
	got2 := c2.Describe()
	if !strings.Contains(got2, "ss -ltnp") {
		t.Fatalf("未解析出进程时应给出排查命令，实际 %q", got2)
	}
	if strings.Contains(got2, "无人占用") || strings.Contains(got2, "未被占用") {
		t.Fatalf("绝不能把「查不出进程」说成「无人占用」：%q", got2)
	}
}
