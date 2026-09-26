package sni

import (
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

// buildClientHello 手工拼一个最小可用的 ClientHello，方便构造边界用例。
// withExt=false 时完全不写扩展段（模拟"没有 SNI 的老客户端/IP 直连"）。
func buildClientHello(sni string, withExt bool) []byte {
	var body bytes.Buffer
	body.Write([]byte{0x03, 0x03})             // legacy_version TLS1.2
	body.Write(bytes.Repeat([]byte{0x11}, 32)) // random
	body.WriteByte(0)                          // session_id 长度 0
	body.Write([]byte{0x00, 0x02, 0x13, 0x01}) // cipher_suites: TLS_AES_128_GCM_SHA256
	body.Write([]byte{0x01, 0x00})             // compression_methods: null

	if withExt {
		var ext bytes.Buffer
		if sni != "" {
			var sn bytes.Buffer
			sn.WriteByte(0x00) // host_name
			sn.WriteByte(byte(len(sni) >> 8))
			sn.WriteByte(byte(len(sni)))
			sn.WriteString(sni)
			var list bytes.Buffer
			list.WriteByte(byte(sn.Len() >> 8))
			list.WriteByte(byte(sn.Len()))
			list.Write(sn.Bytes())

			ext.Write([]byte{0x00, 0x00}) // type server_name
			ext.WriteByte(byte(list.Len() >> 8))
			ext.WriteByte(byte(list.Len()))
			ext.Write(list.Bytes())
		}
		// 再塞一个无关扩展（supported_versions），确保解析器不是只认第一个扩展
		ext.Write([]byte{0x00, 0x2b, 0x00, 0x03, 0x02, 0x03, 0x04})
		body.WriteByte(byte(ext.Len() >> 8))
		body.WriteByte(byte(ext.Len()))
		body.Write(ext.Bytes())
	}

	var hs bytes.Buffer
	hs.WriteByte(0x01) // ClientHello
	hs.WriteByte(byte(body.Len() >> 16))
	hs.WriteByte(byte(body.Len() >> 8))
	hs.WriteByte(byte(body.Len()))
	hs.Write(body.Bytes())

	var rec bytes.Buffer
	rec.WriteByte(0x16)
	rec.Write([]byte{0x03, 0x01})
	rec.WriteByte(byte(hs.Len() >> 8))
	rec.WriteByte(byte(hs.Len()))
	rec.Write(hs.Bytes())
	return rec.Bytes()
}

func TestParseHandcraftedClientHello(t *testing.T) {
	name, err := parseClientHello(buildClientHello("shop.example.com", true))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if name != "shop.example.com" {
		t.Fatalf("SNI 不对: %q", name)
	}
}

func TestParseNoExtensionsMeansNoSNI(t *testing.T) {
	_, err := parseClientHello(buildClientHello("", false))
	if !errors.Is(err, ErrNoSNI) {
		t.Fatalf("无扩展应返回 ErrNoSNI，got %v", err)
	}
	// 有扩展段但没有 server_name 扩展，同样是 ErrNoSNI
	_, err = parseClientHello(buildClientHello("", true))
	if !errors.Is(err, ErrNoSNI) {
		t.Fatalf("无 server_name 应返回 ErrNoSNI，got %v", err)
	}
}

func TestParseRejectsNonTLS(t *testing.T) {
	// 普通 HTTP 请求（模式 B 的 TCP 转发会遇到），必须能明确识别为"不是 TLS"
	b := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	if _, err := parseClientHello(b); !errors.Is(err, ErrNotTLS) {
		t.Fatalf("应返回 ErrNotTLS，got %v", err)
	}
	// 少于 5 字节 → 需要更多数据，而不是判定为非法
	if _, err := parseClientHello([]byte{0x16, 0x03}); !errors.Is(err, errNeedMore) {
		t.Fatalf("短数据应返回 errNeedMore，got %v", err)
	}
}

func TestParseHandlesTruncation(t *testing.T) {
	full := buildClientHello("a.example.com", true)
	// 从任意位置截断，都不允许 panic，且只能是 errNeedMore 或 ErrMalformed
	for i := 1; i < len(full); i++ {
		_, err := parseClientHello(full[:i])
		if err == nil {
			t.Fatalf("截断到 %d 字节竟然解析成功", i)
		}
		if !errors.Is(err, errNeedMore) && !errors.Is(err, ErrMalformed) {
			t.Fatalf("截断到 %d 字节返回了意外错误: %v", i, err)
		}
	}
}

func TestParseRejectsMalformedHostname(t *testing.T) {
	// 注入 CRLF 的 SNI 必须被拒绝：它是"把外部输入拼进内部对象名"的经典入口
	_, err := parseClientHello(buildClientHello("evil\r\nX: y.com", true))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("非法主机名应返回 ErrMalformed，got %v", err)
	}
}

// 用 Go 标准库真实生成的 ClientHello 验证解析器（避免"只对自己拼的样本有效"）。
func TestParseRealTLSClientHello(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	go func() {
		tc := tls.Client(cli, &tls.Config{ServerName: "shop.example.com", InsecureSkipVerify: true})
		_ = tc.Handshake()
	}()

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	deadline := time.Now().Add(5 * time.Second)
	var name string
	var err error
	for time.Now().Before(deadline) {
		_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, rerr := srv.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		name, err = parseClientHello(buf)
		if err == nil || !errors.Is(err, errNeedMore) {
			break
		}
		if rerr != nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("解析真实 ClientHello 失败: %v", err)
	}
	if name != "shop.example.com" {
		t.Fatalf("SNI 不对: %q", name)
	}
}

// PeekSNI 必须把读走的字节原样交还（prefix），否则转发出去的就是残包。
func TestPeekSNIReturnsPrefix(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	want := buildClientHello("relay.example.com", true)
	go func() {
		_, _ = cli.Write(want[:(len(want)+1)/2])
		time.Sleep(20 * time.Millisecond)
		_, _ = cli.Write(want[(len(want)+1)/2:])
	}()

	name, prefix, err := PeekSNI(srv, 3*time.Second)
	if err != nil {
		t.Fatalf("PeekSNI 失败: %v", err)
	}
	if name != "relay.example.com" {
		t.Fatalf("SNI 不对: %q", name)
	}
	if !bytes.HasPrefix(want, prefix) {
		t.Fatalf("prefix 必须是原始字节的前缀；got %d 字节", len(prefix))
	}
	if len(prefix) != len(want) {
		t.Fatalf("应读满整个 ClientHello：got %d want %d", len(prefix), len(want))
	}
}

func TestPeekSNITimesOutOnSilentClient(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	// 客户端连上就不说话 —— 不能把连接永久挂住，也不能 panic
	_, _, err := PeekSNI(srv, 150*time.Millisecond)
	if !errors.Is(err, errNeedMore) {
		t.Fatalf("静默客户端应返回 errNeedMore（可判定为无 SNI），got %v", err)
	}
}

func TestValidHostname(t *testing.T) {
	ok := []string{"a.com", "shop.example.com", "x-y.z", "a_b.c", "xn--fiqs8s.cn"}
	bad := []string{"", ".a.com", "a.com.", "a b.com", "a/b.com", "a\x00b.com", "evil\r\nX: y"}
	for _, h := range ok {
		if !validHostname(h) {
			t.Errorf("%q 应判定为合法", h)
		}
	}
	for _, h := range bad {
		if validHostname(h) {
			t.Errorf("%q 应判定为非法", h)
		}
	}
	if validHostname(string(bytes.Repeat([]byte("a"), 254))) {
		t.Error("超长主机名应判定为非法")
	}
}
