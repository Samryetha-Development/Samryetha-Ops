package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Driver 把请求变成主体。它只负责认证，不负责授权（授权在 perm）。
type Driver interface {
	// Mode 返回驱动种类。
	Mode() Mode
	// Identify 返回请求的主体；ok=false 表示未认证（不写响应）。
	Identify(r *http.Request) (Identity, bool)
	// Challenge 处理未认证请求：API 返回 401，浏览器重定向到登录页。
	Challenge(w http.ResponseWriter, r *http.Request)
	// Mount 注册驱动自身的端点（登录/回调/登出）。非 OIDC 驱动为空实现。
	Mount(mux *http.ServeMux)
	// PublicPaths 是必须免认证的路径（登录端点等）。
	PublicPaths() []string
}

// --- 主体在请求上下文中的传递 ---

type ctxKey struct{}

// WithIdentity 把主体写入上下文（供 syscall 入口读取）。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IdentityFrom 从上下文取主体；不存在时返回零值（= 匿名）。
func IdentityFrom(ctx context.Context) Identity {
	v, _ := ctx.Value(ctxKey{}).(Identity)
	return v
}

// SubjectFrom 是 IdentityFrom 的便捷写法。
func SubjectFrom(ctx context.Context) string { return IdentityFrom(ctx).Subject }

// New 按配置构造驱动。出错时调用方应拒绝启动（配置错误不该被容忍）。
func New(cfg *Config, listen string, logf func(format string, args ...any)) (Driver, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := cfg.Validate(listen); err != nil {
		return nil, err
	}
	switch cfg.Mode {
	case ModeNone:
		return &noneDriver{}, nil
	case ModeHeader:
		return &headerDriver{header: "X-Kernel-Subject", mode: ModeHeader}, nil
	case ModeProxy:
		return &headerDriver{header: "X-Kernel-Authenticated-Subject", mode: ModeProxy}, nil
	case ModeOIDC:
		return newOIDCDriver(cfg, logf), nil
	default:
		return nil, fmt.Errorf("auth: unknown mode %q", cfg.Mode)
	}
}

// Gate 是统一认证闸门：已认证则把主体写入上下文后放行，否则 Challenge。
//
// publicPaths 内的路径直接放行（登录端点、healthz）。
func Gate(d Driver, publicPaths []string, next http.Handler) http.Handler {
	pub := map[string]bool{}
	for _, p := range publicPaths {
		pub[p] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pub[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if id, ok := d.Identify(r); ok {
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
			return
		}
		d.Challenge(w, r)
	})
}

// --- none ---

type noneDriver struct{}

func (noneDriver) Mode() Mode { return ModeNone }
func (noneDriver) Identify(*http.Request) (Identity, bool) {
	// 匿名主体（空 sub）仍然"已认证"，交由策略给出默认角色（通常 viewer）。
	// 这不是放行特权：没有策略映射就永远是 viewer。
	return Identity{}, true
}
func (noneDriver) Challenge(w http.ResponseWriter, r *http.Request) { deny(w, r) }
func (noneDriver) Mount(*http.ServeMux)                             {}
func (noneDriver) PublicPaths() []string                            { return nil }

// --- header / proxy ---

// headerDriver 信任反向代理注入的身份请求头。
//
// 安全性完全依赖"内核不可被公网直连"——Validate 已强制 loopback。
type headerDriver struct {
	header string
	mode   Mode
}

func (d *headerDriver) Mode() Mode { return d.mode }
func (d *headerDriver) Identify(r *http.Request) (Identity, bool) {
	v := strings.TrimSpace(r.Header.Get(d.header))
	return Identity{Subject: v}, v != ""
}
func (d *headerDriver) Challenge(w http.ResponseWriter, r *http.Request) { deny(w, r) }
func (d *headerDriver) Mount(*http.ServeMux)                             {}
func (d *headerDriver) PublicPaths() []string                            { return nil }

// deny 是"无法自助登录"时的统一拒绝：API 用 401 JSON，页面用 401 文本。
//
// 不重定向到登录页：header/proxy 模式下登录由上游代理负责，
// 把用户丢到一个不存在的登录页只会掩盖真实的配置问题。
func deny(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "error": map[string]any{"code": "unauthenticated", "message": "authentication required"},
		})
		return
	}
	http.Error(w, "401 authentication required", http.StatusUnauthorized)
}

// --- oidc ---

type oidcDriver struct {
	cfg    *Config
	client *oidcClient
	signer *signer
	logf   func(format string, args ...any)

	secure        bool
	allowedSubs   map[string]bool
	allowedGroups map[string]bool
	callback      string
	login         string
	logout        string
}

func newOIDCDriver(cfg *Config, logf func(string, ...any)) *oidcDriver {
	allowedSubs := map[string]bool{}
	for _, s := range cfg.AllowedSubs {
		if s = strings.TrimSpace(s); s != "" {
			allowedSubs[s] = true
		}
	}
	allowedGroups := map[string]bool{}
	for _, g := range cfg.AllowedGroups {
		if g = strings.TrimSpace(g); g != "" {
			allowedGroups[g] = true
		}
	}
	return &oidcDriver{
		cfg:           cfg,
		client:        newOIDCClient(cfg),
		signer:        newSigner(cfg.SessionSecret, cfg.ttl()),
		logf:          logf,
		secure:        cfg.UsesHTTPS(),
		allowedSubs:   allowedSubs,
		allowedGroups: allowedGroups,
		callback:      cfg.callbackPath(),
		login:         cfg.loginPath(),
		logout:        cfg.logoutPath(),
	}
}

func (d *oidcDriver) Mode() Mode { return ModeOIDC }

func (d *oidcDriver) PublicPaths() []string {
	return []string{d.login, d.callback, d.logout}
}

func (d *oidcDriver) Identify(r *http.Request) (Identity, bool) {
	c, err := r.Cookie(d.cfg.cookieName())
	if err != nil {
		return Identity{}, false
	}
	id, ok := d.signer.verify(c.Value)
	if !ok {
		return Identity{}, false
	}
	if !d.allowed(id) {
		return Identity{}, false
	}
	return id, true
}

// allowed 是准入闸门：配置了 allowed_subs / allowed_groups 时，必须命中其一。
//
// 两者都没配 = 任何通过 IdP 认证的用户都可进入（角色仍由策略决定，
// 默认 viewer）。给管理控制台建议至少配一个 allowed_groups。
func (d *oidcDriver) allowed(id Identity) bool {
	if len(d.allowedSubs) == 0 && len(d.allowedGroups) == 0 {
		return true
	}
	if d.allowedSubs[id.Subject] {
		return true
	}
	for _, g := range id.Groups {
		if d.allowedGroups[g] {
			return true
		}
	}
	return false
}

func (d *oidcDriver) Challenge(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		deny(w, r)
		return
	}
	next := safeNext(r.URL.RequestURI())
	http.Redirect(w, r, d.login+"?next="+url.QueryEscape(next), http.StatusFound)
}

func (d *oidcDriver) Mount(mux *http.ServeMux) {
	mux.HandleFunc(d.login, d.handleLogin)
	mux.HandleFunc(d.callback, d.handleCallback)
	mux.HandleFunc(d.logout, d.handleLogout)
}

const (
	cookieState    = "_oidc_state"
	cookieVerifier = "_oidc_verifier"
	cookieNext     = "_oidc_next"
)

func (d *oidcDriver) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := randToken(32)
	if err != nil {
		d.fail(w, http.StatusInternalServerError, "cannot generate state")
		return
	}
	verifier, err := randToken(32)
	if err != nil {
		d.fail(w, http.StatusInternalServerError, "cannot generate pkce verifier")
		return
	}
	// 临时 Cookie：回调时用于校验 state 与 PKCE。10 分钟足够一次登录往返。
	d.setCookie(w, cookieState, state, 600)
	d.setCookie(w, cookieVerifier, verifier, 600)
	d.setCookie(w, cookieNext, safeNext(r.URL.Query().Get("next")), 600)

	u, err := d.client.authorizeURL(r.Context(), state, pkceS256(verifier))
	if err != nil {
		d.logf("auth: authorize url: %v", err)
		d.fail(w, http.StatusServiceUnavailable, "identity provider unavailable")
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

func (d *oidcDriver) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		d.fail(w, http.StatusForbidden, "identity provider rejected login: "+e)
		return
	}
	// state 必须与登录时下发的 Cookie 一致：防 CSRF / 登录夹持
	sc, err := r.Cookie(cookieState)
	if err != nil || sc.Value == "" || q.Get("state") != sc.Value {
		d.fail(w, http.StatusBadRequest, "invalid state")
		return
	}
	vc, err := r.Cookie(cookieVerifier)
	if err != nil || vc.Value == "" {
		d.fail(w, http.StatusBadRequest, "missing pkce verifier")
		return
	}
	code := q.Get("code")
	if code == "" {
		d.fail(w, http.StatusBadRequest, "missing code")
		return
	}

	ctx := r.Context()
	token, err := d.client.exchange(ctx, code, vc.Value)
	if err != nil {
		d.logf("auth: token exchange: %v", err)
		d.fail(w, http.StatusBadGateway, "token exchange failed")
		return
	}
	info, err := d.client.userinfo(ctx, token)
	if err != nil {
		d.logf("auth: userinfo: %v", err)
		d.fail(w, http.StatusBadGateway, "could not read user info")
		return
	}
	id := Identity{Subject: info.Sub, Groups: info.Groups}
	if !d.allowed(id) {
		d.logf("auth: subject %s (groups %v) is not allowed", id.Subject, id.Groups)
		d.fail(w, http.StatusForbidden, "this account is not allowed to access the console")
		return
	}

	d.setCookie(w, d.cfg.cookieName(), d.signer.sign(id), int(d.cfg.ttl().Seconds()))
	d.clearCookie(w, cookieState)
	d.clearCookie(w, cookieVerifier)

	next := "/"
	if nc, err := r.Cookie(cookieNext); err == nil {
		next = safeNext(nc.Value)
	}
	d.clearCookie(w, cookieNext)
	d.logf("auth: login ok: sub=%s email=%s groups=%v", id.Subject, info.Email, id.Groups)
	http.Redirect(w, r, next, http.StatusFound)
}

func (d *oidcDriver) handleLogout(w http.ResponseWriter, r *http.Request) {
	d.clearCookie(w, d.cfg.cookieName())
	http.Redirect(w, r, "/", http.StatusFound)
}

// --- 辅助 ---

// safeNext 只允许站内相对路径，防止开放重定向。
func safeNext(next string) string {
	if next == "" {
		return "/"
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}

func (d *oidcDriver) setCookie(w http.ResponseWriter, name, val string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    val,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   d.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (d *oidcDriver) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: d.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (d *oidcDriver) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}
