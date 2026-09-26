package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

// 已知答案测试：PBKDF2-HMAC-SHA256 的公开测试向量。
// 这条测试的作用是防止"为了性能手滑改坏了 KDF"—— 这种 bug 不会让任何功能报错，
// 只会静默地把口令强度降下来，因此必须有确定性向量守着。
func TestPBKDF2SHA256KnownAnswers(t *testing.T) {
	cases := []struct {
		password string
		salt     string
		iter     int
		keyLen   int
		wantHex  string
	}{
		{"password", "salt", 1, 32,
			"120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32,
			"ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32,
			"c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, c := range cases {
		got := pbkdf2SHA256([]byte(c.password), []byte(c.salt), c.iter, c.keyLen)
		if hex.EncodeToString(got) != c.wantHex {
			t.Errorf("PBKDF2(%q, %q, iter=%d, len=%d)\n got %s\nwant %s",
				c.password, c.salt, c.iter, c.keyLen, hex.EncodeToString(got), c.wantHex)
		}
	}
}

func TestHashVerifyRoundTrip(t *testing.T) {
	// 用低迭代跑测试，否则每轮 0.3s 会让整个测试套变慢。
	const iters = 1000
	h, salt, it, err := Hash("Correct-Horse-Battery-9", iters)
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	if it != iters {
		t.Fatalf("迭代次数未透传: got %d want %d", it, iters)
	}
	if h == "" || salt == "" {
		t.Fatal("哈希或盐为空")
	}
	if err := Verify("Correct-Horse-Battery-9", h, salt, it); err != nil {
		t.Fatalf("正确口令校验失败: %v", err)
	}
	if err := Verify("wrong-password", h, salt, it); err != ErrMismatch {
		t.Fatalf("错误口令应返回 ErrMismatch，实际 %v", err)
	}
}

// 同一个口令两次哈希必须不同（盐起作用），否则一次撞库就能批量命中。
func TestHashIsSalted(t *testing.T) {
	h1, s1, _, _ := Hash("same-password", 500)
	h2, s2, _, _ := Hash("same-password", 500)
	if s1 == s2 {
		t.Fatal("两次生成使用了相同的盐")
	}
	if h1 == h2 {
		t.Fatal("两次哈希结果相同，盐没有生效")
	}
}

func TestVerifyRejectsMalformedInput(t *testing.T) {
	for _, c := range []struct{ name, hash, salt string }{
		{"哈希非 base64", "not-base64!!", "AAAAAAAAAAAAAAAAAAAAAA=="},
		{"盐非 base64", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "not-base64!!"},
		{"两者皆空", "", ""},
		{"哈希为空", "", "AAAAAAAAAAAAAAAAAAAAAA=="},
		{"盐为空", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", ""},
		{"哈希过短", "AAAA", "AAAAAAAAAAAAAAAAAAAAAA=="},
	} {
		if err := Verify("x", c.hash, c.salt, 100); err != ErrMismatch {
			t.Errorf("%s：应返回 ErrMismatch，got %v", c.name, err)
		}
	}
}

// 回归测试：空/过短哈希绝不能让任意口令通过。
// 这个漏洞曾真实存在 —— 修复前 Verify("anything","","",n) 返回 nil。
func TestVerifyEmptyHashNeverAuthenticates(t *testing.T) {
	for _, pw := range []string{"", "x", "admin", "任意口令"} {
		for _, c := range []struct{ hash, salt string }{
			{"", ""},
			{"", "AAAAAAAAAAAAAAAAAAAAAA=="},
			{"AAAA", "AAAAAAAAAAAAAAAAAAAAAA=="},
		} {
			if err := Verify(pw, c.hash, c.salt, 100); err == nil {
				t.Fatalf("口令 %q 对空/过短哈希 %q/%q 竟然校验通过", pw, c.hash, c.salt)
			}
		}
	}
}

func TestHashRejectsEmptyPassword(t *testing.T) {
	if _, _, _, err := Hash("   ", 100); err == nil {
		t.Fatal("空白口令应被拒绝")
	}
}

func TestNewTokenUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		tok, err := NewToken(32)
		if err != nil {
			t.Fatalf("NewToken 失败: %v", err)
		}
		if len(tok) != 64 {
			t.Fatalf("32 字节应编码为 64 个 hex 字符，got %d", len(tok))
		}
		if seen[tok] {
			t.Fatal("生成了重复令牌")
		}
		seen[tok] = true
	}
}

func TestTokenHashIsStableAndNotReversible(t *testing.T) {
	tok := "abcdef0123456789"
	h1, h2 := TokenHash(tok), TokenHash(tok)
	if h1 != h2 {
		t.Fatal("TokenHash 不稳定")
	}
	if strings.Contains(h1, tok) {
		t.Fatal("摘要里不应出现原文")
	}
	if len(h1) != 64 {
		t.Fatalf("SHA-256 hex 应为 64 字符，got %d", len(h1))
	}
}

func TestEqual(t *testing.T) {
	if !Equal("a", "a") {
		t.Fatal("相同字符串应判定相等")
	}
	if Equal("a", "b") || Equal("a", "aa") {
		t.Fatal("不同字符串不应判定相等")
	}
}
