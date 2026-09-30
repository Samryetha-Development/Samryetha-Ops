package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// randToken 生成 n 字节的随机令牌（base64url，无填充）。
func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceS256 返回 PKCE 的 code_challenge（S256）。
//
// S256：challenge = base64url(sha256(verifier))。
// 用 S256 而非 plain：plain 会让 verifier 出现在授权请求里，失去 PKCE 的意义。
func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// signer 用 HMAC-SHA256 对会话值签名，格式：base64url(sub|exp).base64url(mac)。
//
// 为什么自己做而不是引入 JWT 库：本仓库坚持零第三方依赖（单静态二进制）。
// 会话只需"防篡改 + 过期"，HMAC 足够；JWT 的额外能力这里用不上。
type signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // 便于测试注入时间
}

func newSigner(secret string, ttl time.Duration) *signer {
	return &signer{secret: []byte(secret), ttl: ttl, now: time.Now}
}

// sign 生成会话值。
func (s *signer) sign(sub string) string {
	exp := s.now().Add(s.ttl).Unix()
	payload := fmt.Sprintf("%s|%d", sub, exp)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.mac(payload)
}

// verify 校验并解析会话值。任何篡改/过期/格式错误都返回 ok=false。
func (s *signer) verify(v string) (string, bool) {
	i := strings.LastIndexByte(v, '.')
	if i <= 0 || i == len(v)-1 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(v[:i])
	if err != nil {
		return "", false
	}
	payload := string(raw)
	// 常量时间比较，避免时序侧信道
	if !hmac.Equal([]byte(s.mac(payload)), []byte(v[i+1:])) {
		return "", false
	}
	j := strings.LastIndexByte(payload, '|')
	if j <= 0 {
		return "", false
	}
	sub := payload[:j]
	exp, err := strconv.ParseInt(payload[j+1:], 10, 64)
	if err != nil {
		return "", false
	}
	if sub == "" || s.now().Unix() > exp {
		return "", false
	}
	return sub, true
}

func (s *signer) mac(payload string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
