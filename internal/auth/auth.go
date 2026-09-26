// Package auth 提供口令哈希、随机令牌与常量时间比较。
//
// 为什么自己实现 PBKDF2 而不引入 golang.org/x/crypto/bcrypt：
//   - PBKDF2 是 HMAC 之上的**构造**，不是密码学原语，实现只有几十行且可逐行审计；
//   - 本产品是私有化交付的中转平台，供应链风险权重很高，
//     每少一个第三方依赖就少一处"被投毒即被控"的面（这一原则写在 go.mod 顶部）；
//   - bcrypt/argon2 的优势（抗 GPU）在"管理员后台、口令强、失败次数受控"的场景下收益有限。
//
// 如果贵方安全要求必须用 argon2id，只需替换本文件并保留 Hash/Verify 签名即可。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// DefaultIterations 生产默认迭代次数。OWASP 对 PBKDF2-HMAC-SHA256 的建议是 600,000。
//
// 这是一个**变量**而不是常量，测试可以调低它以免每次登录都要等半秒；
// 生产启动时应显式断言它没被改小（见 cmd/shengyu-edgelink-server 的自检）。
var DefaultIterations = 600_000

// SaltLen / KeyLen 以字节计。
const (
	SaltLen = 16
	KeyLen  = 32
)

// ErrMismatch 口令不匹配。
var ErrMismatch = errors.New("auth: 口令不匹配")

// Hash 计算口令哈希，返回 (hashB64, saltB64, iterations)。
// iterations<=0 时使用 DefaultIterations。
func Hash(password string, iterations int) (hash, salt string, iters int, err error) {
	if strings.TrimSpace(password) == "" {
		return "", "", 0, errors.New("auth: 口令不能为空")
	}
	iters = iterations
	if iters <= 0 {
		iters = DefaultIterations
	}
	saltBytes := make([]byte, SaltLen)
	if _, err = rand.Read(saltBytes); err != nil {
		return "", "", 0, fmt.Errorf("auth: 生成盐失败: %w", err)
	}
	dk := pbkdf2SHA256([]byte(password), saltBytes, iters, KeyLen)
	return base64.StdEncoding.EncodeToString(dk),
		base64.StdEncoding.EncodeToString(saltBytes),
		iters, nil
}

// MinKeyLen / MinSaltLen 是 Verify 可接受的解码后最短长度。
//
// 这两个下限不是"洁癖"，而是堵一个真实存在的漏洞：
// 若库里某行的 password_hash 是空串（导入脚本写错、迁移漏列、手工 UPDATE），
// base64 解码得到 0 字节，pbkdf2 的 keyLen 就变成 0，于是任何口令算出的都是空切片，
// ConstantTimeCompare([], []) == 1 —— **任意口令都能登录**。
const (
	MinKeyLen  = 20 // 我们生成 32 字节；留余量以兼容日后换成别的 KDF 输出长度
	MinSaltLen = 8  // 我们生成 16 字节
)

// Verify 校验口令。任何解码/格式错误都返回 ErrMismatch（不泄露"是格式错还是口令错"）。
func Verify(password, hashB64, saltB64 string, iterations int) error {
	want, err := base64.StdEncoding.DecodeString(hashB64)
	if err != nil {
		return ErrMismatch
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return ErrMismatch
	}
	// 见 MinKeyLen 的注释：这两行是安全兜底，不是防御性编程。
	if len(want) < MinKeyLen || len(salt) < MinSaltLen {
		return ErrMismatch
	}
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	got := pbkdf2SHA256([]byte(password), salt, iterations, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// pbkdf2SHA256 是 RFC 8018 定义的 PBKDF2，PRF 固定为 HMAC-SHA256。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	if iter < 1 {
		iter = 1
	}
	hLen := sha256.Size
	blocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, blocks*hLen)
	buf := make([]byte, 4)

	for b := 1; b <= blocks; b++ {
		buf[0] = byte(b >> 24)
		buf[1] = byte(b >> 16)
		buf[2] = byte(b >> 8)
		buf[3] = byte(b)

		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write(buf)
		u := mac.Sum(nil)

		t := make([]byte, hLen)
		copy(t, u)

		for i := 1; i < iter; i++ {
			mac.Reset()
			mac.Write(u)
			// Sum(u[:0]) 复用 u 的底层数组：避免每轮分配，行为与标准实现一致。
			u = mac.Sum(u[:0])
			for j := 0; j < hLen; j++ {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// ============================== 随机令牌 ==============================

// NewToken 生成 n 字节的随机令牌（hex 编码）。用于会话令牌、CSRF、节点注册令牌。
func NewToken(n int) (string, error) {
	if n <= 0 {
		n = 32
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: 生成随机令牌失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// TokenHash 是令牌入库前的单向摘要。
//
// 这里用**未加盐的 SHA-256** 而不是 PBKDF2，理由是刻意的：
// 令牌本身就是 256 位随机数，不存在"弱口令字典"，暴力破解不可行；
// 而登录路径每一次都要查库，用 PBKDF2 会让每次请求多花 0.3s。
// 用 SHA-256 可以让 token_hash 直接建唯一索引 —— 一次索引命中完成校验。
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Equal 常量时间比较两个字符串（避免用 == 比较令牌时泄露前缀）。
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
