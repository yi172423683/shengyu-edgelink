// Package id 生成稳定且唯一、时间有序的内部对象 ID。
//
// 设计约束（需求 §四）：
//  1. 业务 ID 必须稳定唯一，且**永远不允许用域名拼接内部对象名**；
//  2. HANDLE 只含 [a-z0-9]，可以直接进 HAProxy 对象名（HAProxy 对象名对字符有限制）；
//  3. 前缀区分对象类型，便于日志与排查时一眼看出类型。
package id

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
)

// crockford 是 Crockford Base32 字母表（去掉 I/L/O/U，避免人工抄写歧义）。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New 生成 `<prefix>_<26位 Crockford Base32>`。
//
// 前 48 bit 是毫秒时间戳（因此字符串排序 == 时间排序），后 82 bit 是随机数。
// 26 个字符 * 5 bit = 130 bit，足够避免碰撞，同时长度固定便于索引。
func New(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		// rand.Read 在正常 Linux 上不会失败；真失败时用纳秒时间兜底，
		// 保证函数永远不会 panic（ID 生成不该成为可用性风险）。
		ns := uint64(time.Now().UnixNano())
		binary.BigEndian.PutUint64(b[8:], ns)
	}
	return prefix + "_" + encode(b[:], 26)
}

// encode 把 src 编码成 n 个 Crockford Base32 字符。
// 按“大端位流”取 5 bit 一组，多出来的低位补 0。
func encode(src []byte, n int) string {
	out := make([]byte, 0, n)
	var acc uint32
	var bits uint
	for _, c := range src {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, crockford[(acc>>bits)&0x1f])
			if len(out) == n {
				return string(out)
			}
		}
	}
	// 收尾：把剩的位左移到 5 bit 对齐再输出。
	for len(out) < n {
		out = append(out, crockford[(acc<<(5-bits))&0x1f])
		bits = 0
	}
	return string(out)
}

// Handle 把任意 ID 转成可安全放进 HAProxy 对象名的短手柄：
// 只保留 [a-z0-9]，截断 16 位。
//
// 关键：**域名绝不参与**。这条规则由 TestHandleNeverUsesDomain 守护。
func Handle(objectID string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(objectID) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		}
	}
	s := sb.String()
	if len(s) > 16 {
		s = s[:16]
	}
	return s
}

// HandleValid 供校验器使用：手柄必须非空且只含 [a-z0-9]。
func HandleValid(h string) bool {
	if h == "" {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// Token 生成一次性注册令牌的明文（只在创建时返回一次，库里只存 SHA-256）。
//
// 用 32 字节随机 + Crockford，避免 Base64 里的 +/ 与 = 在复制粘贴时被截断。
func Token() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("id: 无法获取安全随机数: " + err.Error())
	}
	return "syr_" + encode(b[:], 51)
}
