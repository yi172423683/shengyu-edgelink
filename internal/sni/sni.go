// Package sni 从 TLS ClientHello 中提取 SNI，**不终止 TLS、不持有私钥**。
//
// 为什么需要它：需求 §六.A 明确要求"SNI 需要在握手阶段捕获到适合会话生命周期的变量，
// 不能假设连接结束时还能重新读取 ClientHello"。TCP 层面的转发器只能看到一次字节流，
// 握手字节读过就没了，所以必须在转发之前先缓存下来。
//
// 本包只解析、不改写、不重放握手 —— 它把读到的字节原样交还给调用方（prefix），
// 由调用方先补齐前缀再双向透传。这样客户端与源站之间的 TLS 仍然是端到端加密的。
package sni

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// 错误定义。调用方据此区分"这不是 TLS 流量"与"是 TLS 但握手还没收完"。
var (
	// ErrNotTLS：首字节不是 TLS 握手记录（0x16）。普通 TCP 应用会走到这里。
	ErrNotTLS = errors.New("sni: 不是 TLS 握手流量")
	// ErrNoSNI：是合法 ClientHello，但没有 server_name 扩展（例如用 IP 直连）。
	ErrNoSNI = errors.New("sni: ClientHello 未携带 SNI")
	// ErrMalformed：结构不合法，可能是恶意或损坏的流量。
	ErrMalformed = errors.New("sni: ClientHello 结构不合法")
)

// errNeedMore 内部信号：数据还不够，需要继续读。
var errNeedMore = errors.New("sni: 需要更多数据")

// maxHelloBytes 单次握手解析的字节上限。
//
// 合法 ClientHello 通常 <2KB（带大量扩展也就 4KB 左右）。
// 设一个硬上限是必须的：否则攻击者可以发一个"永远不完整"的 ClientHello 把内存吃干。
const maxHelloBytes = 16 * 1024

// PeekSNI 从 conn 上读取恰好足够的字节来解析 SNI，并返回读到的全部字节 prefix。
//
// 关键契约：
//   - 无论成功还是失败，prefix 都包含**已经从 conn 读走的所有字节**，
//     调用方必须把 prefix 先写给上游，否则会丢数据（或造成握手失败）；
//   - 超时/短读不会破坏连接，只是返回 errNeedMore 包装的错误；
//   - 本函数不写任何字节给 conn，是纯读取操作。
func PeekSNI(conn net.Conn, timeout time.Duration) (sniName string, prefix []byte, err error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	buf := make([]byte, 512)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return "", prefix, fmt.Errorf("sni: 设置读超时失败: %w", err)
		}
		n, rerr := conn.Read(buf)
		if n > 0 {
			prefix = append(prefix, buf[:n]...)
		}
		if len(prefix) > 0 {
			// 每读一段就重试解析：ClientHello 常常跨多个 TCP 段到达。
			name, perr := parseClientHello(prefix)
			switch {
			case perr == nil:
				return name, prefix, nil
			case errors.Is(perr, errNeedMore):
				// 继续读
			case errors.Is(perr, ErrNotTLS), errors.Is(perr, ErrNoSNI), errors.Is(perr, ErrMalformed):
				return "", prefix, perr
			default:
				return "", prefix, perr
			}
		}
		if len(prefix) >= maxHelloBytes {
			// 连 16KB 都凑不出一个合法 ClientHello，判定为畸形，拒绝继续读。
			return "", prefix, fmt.Errorf("%w: 前缀超过 %d 字节仍未解析出 SNI", ErrMalformed, maxHelloBytes)
		}
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				return "", prefix, errNeedMore
			}
			if errors.Is(rerr, io.EOF) {
				return "", prefix, errNeedMore
			}
			return "", prefix, fmt.Errorf("sni: 读取失败: %w", rerr)
		}
	}
}

// parseClientHello 解析 TLS 记录层 + 握手层，取出 SNI。
// 数据不足时返回 errNeedMore（而非错误），便于流式调用。
func parseClientHello(b []byte) (string, error) {
	// ---- TLS 记录层：type(1) version(2) length(2) ----
	if len(b) < 5 {
		return "", errNeedMore
	}
	if b[0] != 0x16 { // handshake
		return "", ErrNotTLS
	}
	// 记录层版本：0x0301 (TLS1.0) 是历史兼容值，TLS1.2/1.3 的 ClientHello 也常这么写。
	if b[1] != 0x03 {
		return "", fmt.Errorf("%w: 记录层版本 %#x", ErrMalformed, b[1])
	}
	recLen := int(b[3])<<8 | int(b[4])
	if recLen == 0 || recLen > maxHelloBytes {
		return "", fmt.Errorf("%w: 记录长度 %d", ErrMalformed, recLen)
	}
	if len(b) < 5+recLen {
		return "", errNeedMore
	}
	rec := b[5 : 5+recLen]

	// ---- 握手层：type(1) length(3) ----
	if len(rec) < 4 {
		return "", errNeedMore
	}
	if rec[0] != 0x01 { // ClientHello
		return "", fmt.Errorf("%w: 握手类型 %#x 不是 ClientHello", ErrMalformed, rec[0])
	}
	hsLen := int(rec[1])<<16 | int(rec[2])<<8 | int(rec[3])
	if hsLen > len(rec)-4 {
		// 握手体超出记录长度：某些客户端会把 ClientHello 分片，这里保守地继续等，
		// 但只有当我们还没收满 maxHelloBytes 时才有意义。
		return "", errNeedMore
	}
	body := rec[4 : 4+hsLen]

	// ---- ClientHello 主体 ----
	// legacy_version(2) random(32)
	if len(body) < 34 {
		return "", errNeedMore
	}
	p := 34
	// session_id: 1 + n
	if p >= len(body) {
		return "", errNeedMore
	}
	sidLen := int(body[p])
	p++
	if p+sidLen > len(body) {
		return "", errNeedMore
	}
	p += sidLen
	// cipher_suites: 2 + n
	if p+2 > len(body) {
		return "", errNeedMore
	}
	csLen := int(body[p])<<8 | int(body[p+1])
	p += 2
	if p+csLen > len(body) {
		return "", errNeedMore
	}
	p += csLen
	// compression_methods: 1 + n
	if p >= len(body) {
		return "", errNeedMore
	}
	cmLen := int(body[p])
	p++
	if p+cmLen > len(body) {
		return "", errNeedMore
	}
	p += cmLen

	// 没有扩展段 = 没有 SNI（老客户端或 IP 直连）。这是合法情况。
	if p == len(body) {
		return "", ErrNoSNI
	}
	if p+2 > len(body) {
		return "", errNeedMore
	}
	extTotal := int(body[p])<<8 | int(body[p+1])
	p += 2
	end := p + extTotal
	if end > len(body) {
		return "", errNeedMore
	}

	for p+4 <= end {
		etype := int(body[p])<<8 | int(body[p+1])
		elen := int(body[p+2])<<8 | int(body[p+3])
		p += 4
		if p+elen > end {
			return "", fmt.Errorf("%w: 扩展长度越界", ErrMalformed)
		}
		if etype == 0x0000 { // server_name
			name, err := parseServerName(body[p : p+elen])
			if err != nil {
				return "", err
			}
			return name, nil
		}
		p += elen
	}
	return "", ErrNoSNI
}

// parseServerName 解析 server_name 扩展体。
func parseServerName(ext []byte) (string, error) {
	if len(ext) < 2 {
		return "", errNeedMore
	}
	listLen := int(ext[0])<<8 | int(ext[1])
	if 2+listLen > len(ext) {
		return "", errNeedMore
	}
	p := 2
	for p+3 <= 2+listLen {
		nameType := ext[p]
		nlen := int(ext[p+1])<<8 | int(ext[p+2])
		p += 3
		if p+nlen > len(ext) {
			return "", fmt.Errorf("%w: server_name 长度越界", ErrMalformed)
		}
		if nameType == 0x00 { // host_name
			host := string(ext[p : p+nlen])
			if !validHostname(host) {
				return "", fmt.Errorf("%w: 非法主机名 %q", ErrMalformed, host)
			}
			return host, nil
		}
		p += nlen
	}
	return "", ErrNoSNI
}

// validHostname 做基础合法性校验。
//
// 注意：这里**只做字符与长度校验，不做 DNS 解析**。
// 需求 §三 明确禁止"依据任意外部 SNI 自动解析并访问任意目标"——
// SNI 必须去我们的路由表里查，查不到就拒绝，绝不能拿它去 dial。
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '.' || c == '_':
		default:
			return false
		}
	}
	return true
}
