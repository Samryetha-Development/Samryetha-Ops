package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// discovery 是 OIDC 的元数据（取所需子集）。
type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

// userInfo 是 userinfo 端点返回的声明（只保留我们真正使用的字段）。
//
// 刻意不解析 email_verified：本内核不按邮箱授权，解析它就等于暗示"邮箱可用于
// 授权"——那正是要避免的误导。保留 email 仅用于登录日志（可审计）。
type userInfo struct {
	Sub    string   `json:"sub"`
	Email  string   `json:"email"`
	Groups []string `json:"groups"`
}

// oidcClient 是与 IdP 交互的最小客户端。
//
// 发现是**惰性**的：内核启动绝不依赖 IdP 可用。否则 Lako 一宕机，
// 内核就连带起不来——控制台正是用来在故障时救场的，不能反而被依赖拖死。
type oidcClient struct {
	cfg  *Config
	http *http.Client
	mu   sync.Mutex
	disc *discovery
}

func newOIDCClient(cfg *Config) *oidcClient {
	return &oidcClient{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}}
}

// discover 返回元数据，带一个短 TTL 的成功缓存。
func (c *oidcClient) discover(ctx context.Context) (*discovery, error) {
	c.mu.Lock()
	if c.disc != nil {
		d := c.disc
		c.mu.Unlock()
		return d, nil
	}
	c.mu.Unlock()

	u := strings.TrimSuffix(c.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: %s returned %d", u, resp.StatusCode)
	}
	var d discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("oidc discovery: bad json: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, fmt.Errorf("oidc discovery: missing authorization/token endpoint")
	}
	c.mu.Lock()
	c.disc = &d
	c.mu.Unlock()
	return &d, nil
}

// authorizeURL 构造授权请求 URL（授权码 + PKCE、防 CSRF 的 state）。
//
// 不含 nonce：我们没有解析 id_token（用 userinfo 取主体），
// nonce 的防重放价值在这里用不上，多余参数只会让人误以为做了校验。
func (c *oidcClient) authorizeURL(ctx context.Context, state, challenge string) (string, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURI)
	q.Set("scope", strings.Join(c.cfg.scopes(), " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return d.AuthorizationEndpoint + sep + q.Encode(), nil
}

// exchange 用授权码换取访问令牌（公共客户端用 PKCE，不发 client_secret）。
func (c *oidcClient) exchange(ctx context.Context, code, verifier string) (string, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURI)
	form.Set("client_id", c.cfg.ClientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.cfg.ClientSecret != "" {
		req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc token: %d %s", resp.StatusCode, truncate(string(body), 300))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("oidc token: bad json: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("oidc token: empty access_token")
	}
	return tok.AccessToken, nil
}

// userinfo 取回主体信息。
//
// 为什么用 userinfo 而不是解析 id_token：解析并验证 id_token 需要 JWT +
// JWKS 验签，而本仓库坚持零第三方依赖；userinfo 让 IdP 来做验签，
// 我们只走 TLS 信任链，等价且实现更小。
func (c *oidcClient) userinfo(ctx context.Context, accessToken string) (*userInfo, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	if d.UserinfoEndpoint == "" {
		return nil, fmt.Errorf("oidc: issuer has no userinfo_endpoint; cannot identify subject")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.UserinfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc userinfo: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc userinfo: %d %s", resp.StatusCode, truncate(string(body), 300))
	}
	var ui userInfo
	if err := json.Unmarshal(body, &ui); err != nil {
		return nil, fmt.Errorf("oidc userinfo: bad json: %w", err)
	}
	if strings.TrimSpace(ui.Sub) == "" {
		return nil, fmt.Errorf("oidc userinfo: empty sub")
	}
	return &ui, nil
}

// endSessionURL 返回 IdP 的登出地址（可选）。
func (c *oidcClient) endSessionURL(ctx context.Context, idTokenHint, postLogout string) string {
	d, err := c.discover(ctx)
	if err != nil || d.EndSessionEndpoint == "" {
		return ""
	}
	q := url.Values{}
	if idTokenHint != "" {
		q.Set("id_token_hint", idTokenHint)
	}
	if postLogout != "" {
		q.Set("post_logout_redirect_uri", postLogout)
	}
	sep := "?"
	if strings.Contains(d.EndSessionEndpoint, "?") {
		sep = "&"
	}
	return d.EndSessionEndpoint + sep + q.Encode()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
