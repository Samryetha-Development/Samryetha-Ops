// Samryetha 状态站后台：OAuth 保护的 /update 管理面板。
//
// 单静态二进制、零第三方依赖（仅标准库）。由 systemd 以 ubuntu 用户运行，
// 监听 127.0.0.1:3030，Caddy 把 status.samryetha.com/update* 与 /api/update*
// 反代到这里；其余路径仍是纯静态状态页（本服务挂了也不影响公开状态页）。
//
// 鉴权：Lako OIDC（授权码 + PKCE，公共客户端），userinfo 的 email 必须在白名单内。
// 会话：HMAC 签名的 HttpOnly cookie；写操作另有 double-submit CSRF。
package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "embed"
)

//go:embed admin.html
var adminHTML string

var adminTmpl = template.Must(template.New("admin").Parse(adminHTML))

type Config struct {
	Listen      string
	Root        string
	RepoURL     string
	Issuer      string
	ClientID    string
	RedirectURI string
	Admins      map[string]bool // 允许的邮箱（兼容/辅助）
	AdminSubs   map[string]bool // 允许的 Lako 用户 UUID（sub）——主凭据，不可变
	Secret      []byte
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig() (*Config, error) {
	root := env("SAMRYETHA_ROOT", "/opt/Samryetha")
	secret, err := os.ReadFile(filepath.Join(root, "status", ".service-secret"))
	if err != nil {
		return nil, fmt.Errorf("read secret: %w", err)
	}
	admins := map[string]bool{}
	for _, e := range strings.Split(env("ADMIN_EMAIL", "tonytao2022@outlook.com"), ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			admins[e] = true
		}
	}
	// sub 白名单（首选）：Lako 的用户 UUID，登录后不可变，攻击者无法通过注册同邮箱冒充。
	subs := map[string]bool{}
	for _, s := range strings.Split(env("ADMIN_SUBS", ""), ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			subs[s] = true
		}
	}
	return &Config{
		Listen:      env("LISTEN", "127.0.0.1:3030"),
		Root:        root,
		RepoURL:     env("REPO_URL", "https://github.com/Samryetha-Development/Samryetha.git"),
		Issuer:      strings.TrimRight(env("OIDC_ISSUER", "https://auth.samryetha.com"), "/"),
		ClientID:    env("OIDC_CLIENT_ID", "samryetha-status"),
		RedirectURI: env("OIDC_REDIRECT_URI", "https://status.samryetha.com/update/callback"),
		Admins:      admins,
		AdminSubs:   subs,
		Secret:      []byte(strings.TrimSpace(string(secret))),
	}, nil
}

// ---------------------------------------------------------------- 基础

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hmacSign(secret []byte, msg string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func b64sha256(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func setCookie(w http.ResponseWriter, name, val string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: val, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runTimeout(sec int, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(sec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tailFile(p string, n int) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func readFileTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readJSONMap(p string) map[string]any {
	out := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// ---------------------------------------------------------------- 会话

type session struct{ Email string }

func (c *Config) makeSession(email string) string {
	exp := time.Now().Add(8 * time.Hour).Unix()
	payload := fmt.Sprintf("%s|%d", email, exp)
	return payload + "|" + hmacSign(c.Secret, payload)
}

func (c *Config) readSession(r *http.Request) *session {
	ck, err := r.Cookie("sess")
	if err != nil || ck.Value == "" {
		return nil
	}
	parts := strings.Split(ck.Value, "|")
	if len(parts) != 3 {
		return nil
	}
	if !hmac.Equal([]byte(parts[2]), []byte(hmacSign(c.Secret, parts[0]+"|"+parts[1]))) {
		return nil
	}
	var exp int64
	_, _ = fmt.Sscanf(parts[1], "%d", &exp)
	if time.Now().Unix() > exp {
		return nil
	}
	return &session{Email: parts[0]}
}

func (c *Config) requireAuth(w http.ResponseWriter, r *http.Request) *session {
	s := c.readSession(r)
	if s == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return nil
	}
	return s
}

func (c *Config) requireCSRF(w http.ResponseWriter, r *http.Request) bool {
	ck, err := r.Cookie("csrf")
	if err != nil || ck.Value == "" || r.Header.Get("X-CSRF") != ck.Value {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "csrf"})
		return false
	}
	return true
}

func (c *Config) guard(w http.ResponseWriter, r *http.Request) *session {
	s := c.requireAuth(w, r)
	if s == nil {
		return nil
	}
	// 所有写操作必须 POST + CSRF。早先的实现对 GET 跳过 CSRF，而路由未限定方法，
	// 导致 GET /api/update/run 之类可被外部链接诱导触发（CSRF）。现在一律要求 POST。
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return nil
	}
	if !c.requireCSRF(w, r) {
		return nil
	}
	return s
}

// guardRead 用于只读接口：仅要求已登录，不限制方法、不要 CSRF。
func (c *Config) guardRead(w http.ResponseWriter, r *http.Request) *session {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return nil
	}
	return c.requireAuth(w, r)
}

// ---------------------------------------------------------------- OAuth

func (c *Config) handleLogin(w http.ResponseWriter, r *http.Request) {
	if c.readSession(r) != nil {
		http.Redirect(w, r, "/update/", http.StatusFound)
		return
	}
	state := randToken(24)
	verifier := randToken(32)
	setCookie(w, "st", state, 600)
	setCookie(w, "pk", verifier, 600)
	q := url.Values{
		"response_type": {"code"}, "client_id": {c.ClientID},
		"redirect_uri": {c.RedirectURI}, "scope": {"openid email profile"},
		"state": {state}, "code_challenge": {b64sha256(verifier)}, "code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, c.Issuer+"/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (c *Config) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "OAuth error: "+e+" "+q.Get("error_description"), http.StatusBadRequest)
		return
	}
	stCk, err1 := r.Cookie("st")
	pkCk, err2 := r.Cookie("pk")
	if err1 != nil || err2 != nil || q.Get("state") == "" || q.Get("state") != stCk.Value {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {q.Get("code")},
		"redirect_uri": {c.RedirectURI}, "client_id": {c.ClientID}, "code_verifier": {pkCk.Value},
	}
	req, _ := http.NewRequest("POST", c.Issuer+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		http.Error(w, "token rejected: "+string(body), http.StatusBadGateway)
		return
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &tr) != nil || tr.AccessToken == "" {
		http.Error(w, "bad token response", http.StatusBadGateway)
		return
	}
	ureq, _ := http.NewRequest("GET", c.Issuer+"/oauth/userinfo", nil)
	ureq.Header.Set("Authorization", "Bearer "+tr.AccessToken)
	uresp, err := http.DefaultClient.Do(ureq)
	if err != nil {
		http.Error(w, "userinfo failed", http.StatusBadGateway)
		return
	}
	defer uresp.Body.Close()
	ubody, _ := io.ReadAll(io.LimitReader(uresp.Body, 1<<20))
	var info map[string]any
	_ = json.Unmarshal(ubody, &info)
	sub, _ := info["sub"].(string)
	sub = strings.TrimSpace(sub)
	email, _ := info["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	emailVerified, _ := info["email_verified"].(bool)

	// 授权判定（两种凭据，满足其一）：
	//   1) sub 命中 ADMIN_SUBS —— 首选。sub 是 Lako 的用户 UUID，创建后不可变，
	//      且不可被他人注册冒充。生产应配置此项。
	//   2) 邮箱命中 ADMIN_EMAIL —— 仅作兼容/回退，且必须 email_verified=true，
	//      否则任何人注册一个未验证的同名邮箱即可取得运维权限。
	authorized := false
	reason := "not in allowlist"
	if sub != "" && c.AdminSubs[sub] {
		authorized = true
	} else if email != "" && c.Admins[email] {
		if emailVerified {
			authorized = true
		} else {
			reason = "email is not verified and sub is not allowlisted"
		}
	}
	if !authorized {
		ident := email
		if ident == "" {
			ident = "(no email)"
		}
		c.audit(email, "login_denied", "sub="+sub+" "+reason)
		http.Error(w, "forbidden: "+ident+" is not an update admin ("+reason+")", http.StatusForbidden)
		return
	}
	setCookie(w, "sess", c.makeSession(email), 8*3600)
	setCookie(w, "csrf", randToken(24), 8*3600)
	clearCookie(w, "st")
	clearCookie(w, "pk")
	c.audit(email, "login", "sub="+sub)
	http.Redirect(w, r, "/update/", http.StatusFound)
}

func (c *Config) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, "sess")
	clearCookie(w, "csrf")
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---------------------------------------------------------------- 审计

func (c *Config) auditPath() string {
	return filepath.Join(c.Root, "status", "data", "admin-actions.jsonl")
}

func (c *Config) audit(email, action, detail string) {
	rec, _ := json.Marshal(map[string]any{"ts": time.Now().Unix(), "email": email, "action": action, "detail": detail})
	f, err := os.OpenFile(c.auditPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(rec, '\n'))
}

func (c *Config) auditList(n int) []map[string]any {
	b, err := os.ReadFile(c.auditPath())
	if err != nil {
		return []map[string]any{}
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]map[string]any, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------- 数据

func (c *Config) updateConfigPath() string {
	return filepath.Join(c.Root, "status", "update-config.json")
}

func (c *Config) readConfig() map[string]any { return readJSONMap(c.updateConfigPath()) }

func (c *Config) writeConfig(cfg map[string]any) error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(c.updateConfigPath(), append(b, '\n'), 0o644)
}

func (c *Config) updateRunning() bool {
	out, _ := run("bash", "-c", "pgrep -f 'update.sh' >/dev/null && echo yes || echo no")
	return strings.TrimSpace(out) == "yes"
}

func (c *Config) rollbackVersions() []map[string]any {
	entries, err := os.ReadDir(filepath.Join(c.Root, "logs", "rollback"))
	if err != nil {
		return []map[string]any{}
	}
	var out []map[string]any
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, _ := e.Info()
		m := map[string]any{"sha": e.Name()}
		if info != nil {
			m["mtime"] = info.ModTime().Unix()
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		mi, _ := out[i]["mtime"].(int64)
		mj, _ := out[j]["mtime"].(int64)
		return mi > mj
	})
	return out
}

func (c *Config) backups() []map[string]any {
	matches, _ := filepath.Glob(filepath.Join(c.Root, "logs", "db-backup-*"))
	var out []map[string]any
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{"name": filepath.Base(m), "mtime": info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool {
		mi, _ := out[i]["mtime"].(int64)
		mj, _ := out[j]["mtime"].(int64)
		return mi > mj
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func (c *Config) pm2Status() map[string]any {
	out, err := runTimeout(8, "bash", "-lc", "pm2 jlist 2>/dev/null")
	if err != nil || strings.TrimSpace(out) == "" {
		return map[string]any{"ok": false}
	}
	var apps []map[string]any
	if json.Unmarshal([]byte(out), &apps) != nil {
		return map[string]any{"ok": false}
	}
	res := map[string]any{}
	for _, a := range apps {
		name, _ := a["name"].(string)
		envm, _ := a["pm2_env"].(map[string]any)
		if name == "" || envm == nil {
			continue
		}
		mem := 0
		if mon, ok := a["monit"].(map[string]any); ok {
			if v, ok := mon["memory"].(float64); ok {
				mem = int(v / 1048576)
			}
		}
		res[name] = map[string]any{"status": envm["status"], "restarts": envm["restart_time"], "mem_mb": mem}
	}
	return map[string]any{"ok": true, "apps": res}
}

func (c *Config) remoteSHA(branch string) string {
	out, err := runTimeout(10, "git", "ls-remote", c.RepoURL, "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func (c *Config) gitLog(dir string, n int) []map[string]string {
	out, err := run("git", "-C", dir, "log", "--oneline", fmt.Sprintf("-%d", n))
	if err != nil {
		return []map[string]string{}
	}
	var res []map[string]string
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln == "" {
			continue
		}
		parts := strings.SplitN(ln, " ", 2)
		m := map[string]string{"sha": parts[0]}
		if len(parts) > 1 {
			m["subject"] = parts[1]
		}
		res = append(res, m)
	}
	return res
}

func (c *Config) envKeys() []string {
	b, err := os.ReadFile(filepath.Join(c.Root, "backend", ".env"))
	if err != nil {
		return []string{}
	}
	var keys []string
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if i := strings.IndexByte(ln, '='); i > 0 && !strings.HasPrefix(ln, "#") {
			keys = append(keys, ln[:i])
		}
	}
	sort.Strings(keys)
	return keys
}

func (c *Config) disk() map[string]any {
	out, _ := run("bash", "-c", "df -P / | tail -1 | awk '{print $2, $3, $5}'")
	f := strings.Fields(out)
	m := map[string]any{}
	if len(f) == 3 {
		m["total_kb"], _ = strconv.Atoi(f[0])
		m["used_kb"], _ = strconv.Atoi(f[1])
		m["use_pct"] = f[2]
	}
	du, _ := run("bash", "-c", "du -sh /opt/Samryetha 2>/dev/null | awk '{print $1}'")
	m["root_du"] = strings.TrimSpace(du)
	db, _ := run("bash", "-c", "du -sh /opt/Samryetha/backend/data 2>/dev/null | awk '{print $1}'")
	m["db_du"] = strings.TrimSpace(db)
	return m
}

func (c *Config) stateJSON() map[string]any {
	root := c.Root
	dev := filepath.Join(root, "..", "Samryetha-dev")
	gitHead, _ := run("git", "-C", root, "rev-parse", "--short", "HEAD")
	devHead, _ := run("git", "-C", dev, "rev-parse", "--short", "HEAD")
	cfg := c.readConfig()
	branchMain := "main"
	branchDev := "dev"
	if m, ok := cfg["main"].(map[string]any); ok {
		if b, ok := m["branch"].(string); ok && b != "" {
			branchMain = b
		}
	}
	if m, ok := cfg["dev"].(map[string]any); ok {
		if b, ok := m["branch"].(string); ok && b != "" {
			branchDev = b
		}
	}
	return map[string]any{
		"config":        cfg,
		"deployed":      readFileTrim(filepath.Join(root, "logs", ".last-deployed")),
		"devDeployed":   readFileTrim(filepath.Join(root, "logs", ".last-deployed-dev")),
		"gitHead":       strings.TrimSpace(gitHead),
		"devGitHead":    strings.TrimSpace(devHead),
		"remoteMain":    c.remoteSHA(branchMain),
		"remoteDev":     c.remoteSHA(branchDev),
		"branchMain":    branchMain,
		"branchDev":     branchDev,
		"updateRunning": c.updateRunning(),
		"pm2":           c.pm2Status(),
		"rollback":      c.rollbackVersions(),
		"backups":       c.backups(),
		"disk":          c.disk(),
		"gitLog":        c.gitLog(root, 8),
		"envKeys":       c.envKeys(),
		"configNotice":  readJSONMap(filepath.Join(root, "status", "data", "config-notice.json")),
		"codeReport":    readJSONMap(filepath.Join(root, "status", "data", "code-report.json")),
		"actions":       c.auditList(50),
		"logTail":       tailFile(filepath.Join(root, "logs", "update.log"), 40),
		"serverTime":    time.Now().Format("2006-01-02 15:04:05"),
	}
}

// ---------------------------------------------------------------- 写操作

func (c *Config) handleConfig(w http.ResponseWriter, r *http.Request) {
	s := c.guard(w, r)
	if s == nil {
		return
	}
	var patch map[string]any
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&patch) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad json"})
		return
	}
	cur := c.readConfig()
	for _, k := range []string{"main", "dev", "notify"} {
		if pv, ok := patch[k].(map[string]any); ok {
			mv, _ := cur[k].(map[string]any)
			if mv == nil {
				mv = map[string]any{}
			}
			for kk, vv := range pv {
				mv[kk] = vv
			}
			cur[k] = mv
		}
	}
	if err := c.writeConfig(cur); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	c.audit(s.Email, "config", fmt.Sprintf("%v", patch))
	writeJSON(w, 200, map[string]any{"ok": true, "config": cur})
}

func (c *Config) spawn(args ...string) (int, error) {
	cmd := exec.Command("bash", args...)
	cmd.Dir = c.Root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func (c *Config) handleRun(w http.ResponseWriter, r *http.Request) {
	s := c.guard(w, r)
	if s == nil {
		return
	}
	if c.updateRunning() {
		writeJSON(w, 409, map[string]any{"error": "an update is already running"})
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	args := []string{filepath.Join(c.Root, "update.sh")}
	if body.Force {
		args = append(args, "--force")
	}
	pid, err := c.spawn(args...)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	c.audit(s.Email, "run", fmt.Sprintf("force=%v pid=%d", body.Force, pid))
	writeJSON(w, 202, map[string]any{"ok": true, "pid": pid})
}

func (c *Config) handleRollback(w http.ResponseWriter, r *http.Request) {
	s := c.guard(w, r)
	if s == nil {
		return
	}
	var body struct {
		SHA string `json:"sha"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if body.SHA == "" || strings.ContainsAny(body.SHA, "/ \t;") {
		writeJSON(w, 400, map[string]any{"error": "bad sha"})
		return
	}
	if c.updateRunning() {
		writeJSON(w, 409, map[string]any{"error": "an update is already running"})
		return
	}
	pid, err := c.spawn(filepath.Join(c.Root, "update.sh"), "--rollback", body.SHA)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	c.audit(s.Email, "rollback", body.SHA+" pid="+fmt.Sprint(pid))
	writeJSON(w, 202, map[string]any{"ok": true})
}

var allowedApps = map[string]bool{
	"samryetha-backend": true, "samryetha-frontend": true,
	"samryetha-dev-backend": true, "samryetha-dev-frontend": true,
}

func (c *Config) handleRestart(w http.ResponseWriter, r *http.Request) {
	s := c.guard(w, r)
	if s == nil {
		return
	}
	var body struct {
		App string `json:"app"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if !allowedApps[body.App] {
		writeJSON(w, 400, map[string]any{"error": "unknown app"})
		return
	}
	out, err := run("bash", "-lc", "pm2 restart "+body.App)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error(), "out": out})
		return
	}
	c.audit(s.Email, "restart", body.App)
	writeJSON(w, 200, map[string]any{"ok": true, "out": out})
}

func (c *Config) handleStatusRegen(w http.ResponseWriter, r *http.Request) {
	s := c.guard(w, r)
	if s == nil {
		return
	}
	out, err := run("bash", filepath.Join(c.Root, "status", "generate.sh"))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error(), "out": out})
		return
	}
	c.audit(s.Email, "status-regen", "")
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (c *Config) handleCodecheck(w http.ResponseWriter, r *http.Request) {
	s := c.guard(w, r)
	if s == nil {
		return
	}
	out, err := runTimeout(120, "node", filepath.Join(c.Root, "status", "codecheck.mjs"))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error(), "out": out})
		return
	}
	c.audit(s.Email, "codecheck", "")
	writeJSON(w, 200, map[string]any{"ok": true, "report": readJSONMap(filepath.Join(c.Root, "status", "data", "code-report.json"))})
}

func (c *Config) handleBackup(w http.ResponseWriter, r *http.Request) {
	s := c.guard(w, r)
	if s == nil {
		return
	}
	ts := time.Now().Format("20060102-150405")
	dir := filepath.Join(c.Root, "logs", "db-backup-"+ts)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	script := `import sqlite3,sys
src=sqlite3.connect(sys.argv[1]); dst=sqlite3.connect(sys.argv[2])
with dst: src.backup(dst)
dst.close(); src.close()`
	out, err := run("python3", "-c", script,
		filepath.Join(c.Root, "backend", "data", "app.db"), filepath.Join(dir, "app.db"))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error(), "out": out})
		return
	}
	c.audit(s.Email, "backup", ts)
	writeJSON(w, 200, map[string]any{"ok": true, "dir": dir})
}

func (c *Config) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	s := c.guard(w, r)
	if s == nil {
		return
	}
	cfg := c.readConfig()
	hook := ""
	if n, ok := cfg["notify"].(map[string]any); ok {
		hook, _ = n["webhook"].(string)
	}
	if hook == "" {
		writeJSON(w, 400, map[string]any{"error": "未配置 webhook"})
		return
	}
	body := `{"text":"[Samryetha update][test] 这是一条测试告警"}`
	req, _ := http.NewRequest("POST", hook, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}
	resp.Body.Close()
	c.audit(s.Email, "notify-test", hook)
	writeJSON(w, 200, map[string]any{"ok": true, "status": resp.StatusCode})
}

var logFiles = map[string]string{
	"update":       "logs/update.log",
	"backend":      "logs/backend.log",
	"frontend":     "logs/frontend.log",
	"dev-backend":  "logs/dev-backend.log",
	"dev-frontend": "logs/dev-frontend.log",
	"status-gen":   "logs/status-gen.log",
}

func (c *Config) handleLog(w http.ResponseWriter, r *http.Request) {
	if c.guardRead(w, r) == nil {
		return
	}
	rel := logFiles[r.URL.Query().Get("file")]
	if rel == "" {
		rel = logFiles["update"]
	}
	n := 300
	if v := r.URL.Query().Get("lines"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &n)
	}
	if n < 1 || n > 3000 {
		n = 300
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, tailFile(filepath.Join(c.Root, rel), n))
}

// ---------------------------------------------------------------- WebSocket 实时推送
//
// 手写 RFC6455（仅服务端、文本帧、无扩展），零依赖。后台页面通过 /api/update/ws 订阅，
// 服务端每秒采样一次状态，仅在「内容变化」或心跳超时（15s）时推送，尽量省流量。

const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsMagic))
	return base64.StdEncoding.EncodeToString(h[:])
}

// wsWriteText 写一个未分片的文本帧（服务端帧不加掩码）。
func wsWriteText(w io.Writer, payload []byte) error {
	hdr := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// wsReadFrame 读一帧并返回 payload（客户端帧必带掩码）。仅用于读客户端心跳/关闭。
func wsReadFrame(r *bufio.Reader) (opcode byte, payload []byte, err error) {
	h := make([]byte, 2)
	if _, err = io.ReadFull(r, h); err != nil {
		return
	}
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := int(h[1] & 0x7f)
	if n == 126 {
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return
		}
		n = int(binary.BigEndian.Uint16(ext))
	} else if n == 127 {
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return
		}
		n = int(binary.BigEndian.Uint64(ext))
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(r, mask[:]); err != nil {
			return
		}
	}
	if n > 1<<20 {
		err = fmt.Errorf("frame too large")
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

func (c *Config) handleWS(w http.ResponseWriter, r *http.Request) {
	if c.readSession(r) == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" {
		http.Error(w, "expected websocket", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"
	if _, err := buf.WriteString(resp); err != nil || buf.Flush() != nil {
		return
	}

	// 客户端读循环：处理 ping/close 与心跳；断开即退出。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			op, pl, err := wsReadFrame(buf.Reader)
			if err != nil {
				return
			}
			if op == 0x8 { // close
				return
			}
			if op == 0x9 { // ping → pong
				_ = wsWriteText(conn, pl)
			}
		}
	}()

	writeMu := &sync.Mutex{}
	send := func(v any) error {
		b, _ := json.Marshal(v)
		writeMu.Lock()
		defer writeMu.Unlock()
		return wsWriteText(conn, b)
	}

	if err := send(map[string]any{"type": "hello", "state": c.stateJSON()}); err != nil {
		return
	}
	last := ""
	lastSent := time.Now()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			st := c.stateJSON()
			b, _ := json.Marshal(st)
			cur := string(b)
			// 变化即推；无变化每 15s 推一次心跳，保证连接与时钟新鲜。
			if cur != last || time.Since(lastSent) >= 15*time.Second {
				last = cur
				lastSent = time.Now()
				if err := send(map[string]any{"type": "state", "state": st}); err != nil {
					return
				}
			}
		}
	}
}

// ---------------------------------------------------------------- 路由

func (c *Config) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/update/callback", c.handleCallback)
	mux.HandleFunc("/update/logout", c.handleLogout)
	mux.HandleFunc("/update/", func(w http.ResponseWriter, r *http.Request) {
		s := c.readSession(r)
		if s == nil {
			c.handleLogin(w, r)
			return
		}
		csrf := ""
		if ck, err := r.Cookie("csrf"); err == nil {
			csrf = ck.Value
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = adminTmpl.Execute(w, map[string]any{"Email": s.Email, "CSRF": csrf})
	})
	mux.HandleFunc("/update", c.handleLogin)
	mux.HandleFunc("/api/update/state", func(w http.ResponseWriter, r *http.Request) {
		if c.guardRead(w, r) == nil {
			return
		}
		writeJSON(w, 200, c.stateJSON())
	})
	mux.HandleFunc("/api/update/config", c.handleConfig)
	mux.HandleFunc("/api/update/run", c.handleRun)
	mux.HandleFunc("/api/update/rollback", c.handleRollback)
	mux.HandleFunc("/api/update/restart", c.handleRestart)
	mux.HandleFunc("/api/update/status-regen", c.handleStatusRegen)
	mux.HandleFunc("/api/update/codecheck", c.handleCodecheck)
	mux.HandleFunc("/api/update/backup", c.handleBackup)
	mux.HandleFunc("/api/update/notify-test", c.handleNotifyTest)
	mux.HandleFunc("/api/update/log", c.handleLog)
	mux.HandleFunc("/api/update/ws", c.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	return mux
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: cfg.handler(), ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf("samryetha-status on %s (issuer %s, admins %v, subs %v)\n",
		cfg.Listen, cfg.Issuer, cfg.Admins, cfg.AdminSubs)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}
