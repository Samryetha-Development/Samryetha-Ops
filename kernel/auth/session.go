package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Identity 是一个已认证的主体：不可变标识 + 其所属组。
//
// 组来自 IdP 的 groups claim（如 Lako 角色名）。授权层（perm）用组来做
// "给某类人一个角色"，从而让加人这件事发生在 IdP 而无需改内核配置。
type Identity struct {
	Subject string   `json:"s"`
	Groups  []string `json:"g,omitempty"`
}

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

// sessionPayload 是写进 Cookie 的内容。
type sessionPayload struct {
	Subject string   `json:"s"`
	Groups  []string `json:"g,omitempty"`
	Exp     int64    `json:"e"`
}

// signer 用 HMAC-SHA256 对会话做签名，格式：base64url(json).base64url(mac)。
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

// sign 生成会话值。用 JSON 而非分隔符拼接：组名不受分隔符限制。
func (s *signer) sign(id Identity) string {
	p := sessionPayload{Subject: id.Subject, Groups: id.Groups, Exp: s.now().Add(s.ttl).Unix()}
	b, _ := json.Marshal(p)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + s.mac(body)
}

// verify 校验并解析会话。任何篡改/过期/格式错误都返回 ok=false。
func (s *signer) verify(v string) (Identity, bool) {
	i := strings.LastIndexByte(v, '.')
	if i <= 0 || i == len(v)-1 {
		return Identity{}, false
	}
	body := v[:i]
	// 常量时间比较，避免时序侧信道
	if !hmac.Equal([]byte(s.mac(body)), []byte(v[i+1:])) {
		return Identity{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Identity{}, false
	}
	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Identity{}, false
	}
	if p.Subject == "" || s.now().Unix() > p.Exp {
		return Identity{}, false
	}
	return Identity{Subject: p.Subject, Groups: p.Groups}, true
}

func (s *signer) mac(body string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
