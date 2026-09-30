package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- 配置校验 ---

func TestValidateRejectsHeaderModeOnPublicListen(t *testing.T) {
	cfg := &Config{Mode: ModeHeader}
	// 这是真实事故的防线：header 模式 + 公网监听 = 任何人自带头就能冒充管理员
	for _, listen := range []string{"0.0.0.0:3040", ":3040", "10.0.0.5:3040"} {
		if err := cfg.Validate(listen); err == nil {
			t.Fatalf("listen=%q 下 header 模式必须被拒绝", listen)
		}
	}
	if err := cfg.Validate("127.0.0.1:3040"); err != nil {
		t.Fatalf("loopback 监听下 header 模式应当允许：%v", err)
	}
}

func TestValidateOIDCRequirements(t *testing.T) {
	base := Config{Mode: ModeOIDC, Issuer: "https://idp", ClientID: "c", RedirectURI: "https://x/cb"}
	base.SessionSecret = strings.Repeat("a", 32)
	if err := base.Validate("127.0.0.1:1"); err != nil {
		t.Fatalf("完整配置应当通过：%v", err)
	}

	weak := base
	weak.SessionSecret = "short"
	if err := weak.Validate("127.0.0.1:1"); err == nil {
		t.Fatal("过短的 session_secret 必须被拒绝（否则会话可被伪造）")
	}

	noSecret := base
	noSecret.SessionSecret = ""
	if err := noSecret.Validate("127.0.0.1:1"); err == nil {
		t.Fatal("缺少 session_secret 必须被拒绝")
	}

	httpURI := base
	httpURI.RedirectURI = "http://status.example.com/update/callback"
	if err := httpURI.Validate("127.0.0.1:1"); err == nil {
		t.Fatal("非 loopback 的 http 回调必须被拒绝")
	}
}

func TestUnknownModeRejected(t *testing.T) {
	cfg := &Config{Mode: Mode("ldap")}
	if err := cfg.Validate("127.0.0.1:1"); err == nil {
		t.Fatal("未知 mode 必须被拒绝")
	}
}

// 默认 scope 必须包含 groups，否则组授权会静默失效。
func TestDefaultScopesIncludeGroups(t *testing.T) {
	c := &Config{}
	found := false
	for _, s := range c.scopes() {
		if s == "groups" {
			found = true
		}
	}
	if !found {
		t.Fatalf("默认 scope 必须包含 groups，实际 %v", c.scopes())
	}
}

// --- 会话签名 ---

func TestSignerRoundTripAndTamper(t *testing.T) {
	s := newSigner(strings.Repeat("k", 32), time.Hour)
	v := s.sign(Identity{Subject: "sub-1", Groups: []string{"samryetha-admins", "samryetha-users"}})

	id, ok := s.verify(v)
	if !ok || id.Subject != "sub-1" || len(id.Groups) != 2 || id.Groups[0] != "samryetha-admins" {
		t.Fatalf("签名校验失败：%+v ok=%v", id, ok)
	}
	// 篡改签名：改动最后一个字符（会话串是 base64url，不含明文 sub）
	flip := byte('A')
	if v[len(v)-1] == 'A' {
		flip = 'B'
	}
	if _, ok := s.verify(v[:len(v)-1] + string(flip)); ok {
		t.Fatal("被篡改的会话必须被拒绝")
	}
	// 换密钥（伪造）
	if _, ok := newSigner(strings.Repeat("z", 32), time.Hour).verify(v); ok {
		t.Fatal("其他密钥签发的会话必须被拒绝")
	}
	// 过期
	expired := newSigner(strings.Repeat("k", 32), -time.Minute)
	if _, ok := s.verify(expired.sign(Identity{Subject: "sub-1"})); ok {
		t.Fatal("过期会话必须被拒绝")
	}
}

// RFC 7636 附录 B 的 S256 测试向量。
func TestPKCES256Vector(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := pkceS256(verifier); got != want {
		t.Fatalf("S256(verifier)=%q，期望 %q", got, want)
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"/update":            "/update",
		"/":                  "/",
		"":                   "/",
		"//evil.com":         "/",
		"https://evil.com/x": "/",
		"/a\\b":              "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// --- 闸门 ---

func TestNoneDriverPassesEveryoneAsAnonymous(t *testing.T) {
	d, err := New(&Config{Mode: ModeNone}, "127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := d.Identify(httptest.NewRequest("GET", "/x", nil))
	if !ok || id.Subject != "" {
		t.Fatalf("none 模式应给出匿名主体：%+v ok=%v", id, ok)
	}
}

func TestGateChallengesAnonymous(t *testing.T) {
	d := &oidcDriver{cfg: &Config{Mode: ModeOIDC}, signer: newSigner(strings.Repeat("k", 32), time.Hour),
		login: "/update/auth/login", secure: false}
	called := false
	h := Gate(d, []string{"/healthz"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	// API 路径：401 JSON，不重定向
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/kernel/meta", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 API 应当 401，实际 %d", rec.Code)
	}
	if called {
		t.Fatal("未认证请求不应到达内层处理器")
	}

	// 页面路径：重定向到登录
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/update", nil))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/update/auth/login") {
		t.Fatalf("匿名页面应重定向到登录，实际 %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// 公开路径放行
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !called {
		t.Fatalf("/healthz 应当放行，实际 %d called=%v", rec.Code, called)
	}
}

// --- 端到端 OIDC（假 IdP）---

type fakeIdP struct {
	*httptest.Server
	challengeByCode map[string]string
	userinfoSub     string
	userinfoGroups  []string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{challengeByCode: map[string]string{}, userinfoSub: "sub-123"}
	mux := http.NewServeMux()
	base := "" // server 启动后回填
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/oauth/authorize",
			"token_endpoint":         base + "/oauth/token",
			"userinfo_endpoint":      base + "/oauth/userinfo",
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" {
			http.Error(w, "bad grant", 400)
			return
		}
		code := r.Form.Get("code")
		want, ok := f.challengeByCode[code]
		if !ok || want != pkceS256(r.Form.Get("code_verifier")) {
			http.Error(w, "pkce mismatch", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + code, "token_type": "Bearer"})
	})
	mux.HandleFunc("/oauth/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
			http.Error(w, "bad token", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": f.userinfoSub, "email": "tyc@example.com", "email_verified": true,
			"groups": f.userinfoGroups,
		})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	f.Server = srv
	t.Cleanup(srv.Close)
	return f
}

func noRedirectClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// 走完整登录流程并返回已建立会话的客户端 + 服务端地址。
func loginAs(t *testing.T, idp *fakeIdP, cfg *Config, groups []string) (*http.Client, string) {
	t.Helper()
	idp.userinfoGroups = groups
	d, err := New(cfg, "127.0.0.1:3040", t.Logf)
	if err != nil {
		t.Fatalf("构造驱动：%v", err)
	}
	mux := http.NewServeMux()
	d.Mount(mux)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/api/kernel/meta", func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFrom(r.Context())
		_, _ = fmt.Fprintf(w, "subject=%s groups=%v", id.Subject, id.Groups)
	})
	publicPaths := append([]string{"/healthz"}, d.PublicPaths()...)
	srv := httptest.NewServer(Gate(d, publicPaths, mux))
	t.Cleanup(srv.Close)

	c := noRedirectClient()
	resp, err := c.Get(srv.URL + "/update/auth/login?next=/api/kernel/meta")
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(authURL.String(), idp.URL+"/oauth/authorize") {
		t.Fatalf("应重定向到 IdP 授权端点，实际 %q", authURL.String())
	}
	q := authURL.Query()
	if q.Get("state") == "" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("授权请求缺少 state/PKCE：%q", authURL.RawQuery)
	}
	if q.Get("redirect_uri") != cfg.RedirectURI {
		t.Fatalf("redirect_uri 必须是已登记的值，实际 %q", q.Get("redirect_uri"))
	}
	idp.challengeByCode["code-1"] = q.Get("code_challenge")

	// state 不匹配必须被拒（CSRF 防线）
	resp, err = c.Get(srv.URL + "/update/callback?code=code-1&state=WRONG")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("错误 state 应 400，实际 %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = c.Get(srv.URL + "/update/callback?code=code-1&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/api/kernel/meta" {
		t.Fatalf("成功回调应 302 到 next，实际 %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return c, srv.URL
}

func TestOIDCLoginFlowWithGroups(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := &Config{
		Mode:          ModeOIDC,
		Issuer:        idp.URL,
		ClientID:      "samryetha-status",
		RedirectURI:   "http://127.0.0.1/update/callback",
		SessionSecret: strings.Repeat("s", 40),
		// 准入按 Lako 组：这正是"在 IdP 里选用户"的落点
		AllowedGroups: []string{"samryetha-admins"},
	}
	c, base := loginAs(t, idp, cfg, []string{"samryetha-users", "samryetha-admins"})

	// 带会话访问受保护 API → 200，且组被完整带到处理器
	resp, err := c.Get(base + "/api/kernel/meta")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	got := string(body[:n])
	if resp.StatusCode != 200 || !strings.Contains(got, "subject=sub-123") || !strings.Contains(got, "samryetha-admins") {
		t.Fatalf("带会话应可访问且带组，实际 %d %q", resp.StatusCode, got)
	}

	// 伪造会话 → 401
	req, _ := http.NewRequest("GET", base+"/api/kernel/meta", nil)
	req.AddCookie(&http.Cookie{Name: cfg.cookieName(), Value: "forged.value"})
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("伪造会话应 401，实际 %d", resp3.StatusCode)
	}
	resp3.Body.Close()
}

// 不在 allowed_groups 里的主体即使完成 IdP 登录也必须被拒。
func TestOIDCRejectsDisallowedGroup(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := &Config{
		Mode: ModeOIDC, Issuer: idp.URL, ClientID: "c",
		RedirectURI:   "http://127.0.0.1/update/callback",
		SessionSecret: strings.Repeat("s", 40),
		AllowedGroups: []string{"samryetha-admins"},
	}
	d, _ := New(cfg, "127.0.0.1:1", t.Logf)
	mux := http.NewServeMux()
	d.Mount(mux)
	srv := httptest.NewServer(Gate(d, d.PublicPaths(), mux))
	defer srv.Close()

	c := noRedirectClient()
	resp, err := c.Get(srv.URL + "/update/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	idp.challengeByCode["c1"] = authURL.Query().Get("code_challenge")

	idp.userinfoGroups = []string{"samryetha-users"} // 不在准入组里
	resp, err = c.Get(srv.URL + "/update/callback?code=c1&state=" + url.QueryEscape(authURL.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未授权组应 403，实际 %d", resp.StatusCode)
	}
}
