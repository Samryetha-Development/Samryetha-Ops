// Package auth 是内核的认证驱动层：把"请求"变成"主体（subject）"。
//
// 与权限分层的关系（见 kernel/perm）：
//   - auth 只回答"你是谁"（认证）。它**绝不**决定"你能做什么"。
//   - perm 回答"你能做什么"（授权），且角色只由策略按 subject 决定。
//
// 为什么把这两种模式分开成驱动：内核不应假定存在某个 IdP。
// 本包提供 none / header / proxy / oidc 四种驱动，装配层按配置选择；
// 换认证方式不动内核其余部分。
//
// 事故背景：迁移到内核时曾丢失旧控制台的 OIDC，退化成 `mode = header`
// （信任请求头 X-Kernel-Subject）。而 Caddy 把控制台暴露到公网，
// 于是"任何人自带一个 sub 头就能自称管理员"。教训：**任何基于请求头的
// 身份来源，只在监听地址不可公网直达时才成立**——因此 header/proxy
// 模式在非 loopback 监听下会直接拒绝启动（见 Config.Validate）。
package auth

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Mode 是认证驱动的种类。
type Mode string

const (
	// ModeNone：不做认证，所有请求都是匿名主体（走策略默认角色，通常 viewer）。
	// 适用于救援/本地调试。**不要**在公网可达时使用。
	ModeNone Mode = "none"
	// ModeHeader：信任反向代理注入的请求头 X-Kernel-Subject。
	// 仅当监听地址为 loopback（公网无法直连内核）时才允许。
	ModeHeader Mode = "header"
	// ModeProxy：信任 X-Kernel-Authenticated-Subject（与 header 同源，语义更明确）。
	ModeProxy Mode = "proxy"
	// ModeOIDC：授权码 + PKCE 登录，会话以签名 Cookie 维持。
	ModeOIDC Mode = "oidc"
)

// 默认值。集中在此，避免散落在各处导致"改了这里没改那里"。
const (
	defaultCookieName = "samryetha_kernel_session"
	defaultSessionTTL = 12 * time.Hour
	defaultLoginPath  = "/update/auth/login"
	defaultLogoutPath = "/update/auth/logout"
	defaultCallback   = "/update/callback"
	// authPathPrefix 是登录相关端点的前缀。
	// 默认落在 /update/auth 之下：Caddy 已经只把 /update* 与 /api/kernel*
	// 反代到内核，因此这样可以**零基础设施改动**接入登录。
	authPathPrefix = "/update/auth"
)

// Config 是认证配置（来自 etc/auth.json，或环境变量覆盖）。
type Config struct {
	Mode Mode `json:"mode"`

	// --- OIDC ---
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"` // 公共客户端留空（走 PKCE）
	RedirectURI  string   `json:"redirect_uri"`
	Scopes       []string `json:"scopes,omitempty"`

	// AllowedSubs 若非空，则只有这些 sub 能登录（粗粒度闸门）。
	// 授权（角色）仍由 policy 决定；这里只是限制"谁能进门"。
	AllowedSubs []string `json:"allowed_subs,omitempty"`
	// AdminSubs 是便捷项：这些 sub 直接映射为 admin（policy 里已有的映射优先）。
	AdminSubs []string `json:"admin_subs,omitempty"`
	// AdminEmails 是可选的邮箱回退：仅当 email_verified=true 时才生效。
	// 默认不用邮箱授权（邮箱可变、可被抢注），因此通常是空的。
	AdminEmails []string `json:"admin_emails,omitempty"`

	SessionSecret string `json:"session_secret"`
	CookieName    string `json:"cookie_name,omitempty"`
	SessionTTLh   int    `json:"session_ttl_hours,omitempty"`
}

// ttl 返回会话有效期（默认 12 小时）。
func (c *Config) ttl() time.Duration {
	if c.SessionTTLh > 0 {
		return time.Duration(c.SessionTTLh) * time.Hour
	}
	return defaultSessionTTL
}

func (c *Config) cookieName() string {
	if strings.TrimSpace(c.CookieName) != "" {
		return c.CookieName
	}
	return defaultCookieName
}

func (c *Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return []string{"openid", "email", "profile"}
}

// loginPath 返回登录入口路径。
func (c *Config) loginPath() string { return defaultLoginPath }

// logoutPath 返回登出路径。
func (c *Config) logoutPath() string { return defaultLogoutPath }

// callbackPath 由 redirect_uri 的路径部分推导。
//
// 为什么从 redirect_uri 推导而不是另配一个字段：OIDC 要求回调路径与
// 登记在 IdP 的 redirect_uri 完全一致。分开配置迟早会不一致，
// 那会导致"登录后 404"，且报错发生在 IdP 侧，很难定位。
func (c *Config) callbackPath() string {
	p := pathOf(c.RedirectURI)
	if p == "" {
		return defaultCallback
	}
	return p
}

func pathOf(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return ""
	}
	rest := raw[i+3:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return ""
	}
	p := rest[j:]
	if k := strings.IndexAny(p, "?#"); k >= 0 {
		p = p[:k]
	}
	return p
}

// Load 读取认证配置。
//
// 优先级：环境变量 > etc/auth.json > etc/auth.txt。
//
// 为什么保留 auth.txt：它是旧配置格式（只有 mode）；升级期间两者可能并存，
// 迁移成 auth.json 之前不能直接拒绝启动。
func Load(root string) (*Config, error) {
	c := &Config{Mode: ModeNone}

	// 1) auth.json
	if b, err := os.ReadFile(filepath.Join(root, "etc", "auth.json")); err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("auth.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("auth.json: %w", err)
	}

	// 2) auth.txt（仅在没有 auth.json 时作为 mode 来源）
	if _, err := os.Stat(filepath.Join(root, "etc", "auth.json")); os.IsNotExist(err) {
		if m := modeFromTxt(filepath.Join(root, "etc", "auth.txt")); m != "" {
			c.Mode = Mode(m)
		}
	}

	// 3) 环境变量覆盖（便于容器/CI 注入，不必把密钥写进文件）
	if v := os.Getenv("KERNEL_AUTH_MODE"); v != "" {
		c.Mode = Mode(v)
	}
	if v := os.Getenv("KERNEL_OIDC_ISSUER"); v != "" {
		c.Issuer = v
	}
	if v := os.Getenv("KERNEL_OIDC_CLIENT_ID"); v != "" {
		c.ClientID = v
	}
	if v := os.Getenv("KERNEL_OIDC_REDIRECT_URI"); v != "" {
		c.RedirectURI = v
	}
	if v := os.Getenv("KERNEL_OIDC_CLIENT_SECRET"); v != "" {
		c.ClientSecret = v
	}
	if v := os.Getenv("KERNEL_SESSION_SECRET"); v != "" {
		c.SessionSecret = v
	}

	if c.Mode == "" {
		c.Mode = ModeNone
	}
	return c, nil
}

func modeFromTxt(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "mode") {
			if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// IsLoopback 判断监听地址是否只在本机可达。
//
// 这是 header/proxy 模式的安全前提：只要内核能被公网直连，
// 请求头就不可信，"自带 sub 头自称管理员"的漏洞就会重现。
func IsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// 形如 "127.0.0.1"（无端口）或非法值：按不可信处理
		host = listen
	}
	switch strings.Trim(host, "[]") {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// Validate 在启动前校验配置。宁可不启动，也不要带着危险配置上线。
func (c *Config) Validate(listen string) error {
	switch c.Mode {
	case ModeNone:
		return nil
	case ModeHeader, ModeProxy:
		if !IsLoopback(listen) {
			return fmt.Errorf(
				"auth: mode=%s trusts a request header for identity, but the kernel listens on %q "+
					"(not loopback). Anyone who can reach the kernel could impersonate any subject. "+
					"Use mode=oidc, or bind to 127.0.0.1 and put a trusted proxy in front",
				c.Mode, listen)
		}
		return nil
	case ModeOIDC:
		if strings.TrimSpace(c.Issuer) == "" {
			return fmt.Errorf("auth: oidc requires issuer")
		}
		if strings.TrimSpace(c.ClientID) == "" {
			return fmt.Errorf("auth: oidc requires client_id")
		}
		if strings.TrimSpace(c.RedirectURI) == "" {
			return fmt.Errorf("auth: oidc requires redirect_uri")
		}
		if !strings.HasPrefix(c.RedirectURI, "https://") && !IsLoopbackURI(c.RedirectURI) {
			return fmt.Errorf("auth: oidc redirect_uri must be https (got %q)", c.RedirectURI)
		}
		if len(c.SessionSecret) < 32 {
			return fmt.Errorf("auth: oidc requires session_secret of at least 32 chars (got %d); "+
				"a weak secret lets anyone forge a session cookie", len(c.SessionSecret))
		}
		return nil
	default:
		return fmt.Errorf("auth: unknown mode %q (want none|header|proxy|oidc)", c.Mode)
	}
}

// IsLoopbackURI 判断回调地址是否指向本机（本地开发用 http 才可接受）。
func IsLoopbackURI(raw string) bool {
	rest := raw
	if i := strings.Index(raw, "://"); i >= 0 {
		rest = raw[i+3:]
	}
	host := rest
	if j := strings.IndexByte(host, '/'); j >= 0 {
		host = host[:j]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	switch strings.Trim(host, "[]") {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// UsesHTTPS 报告回调是否为 https（决定 Cookie 是否加 Secure）。
func (c *Config) UsesHTTPS() bool { return strings.HasPrefix(c.RedirectURI, "https://") }
