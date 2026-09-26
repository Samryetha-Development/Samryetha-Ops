// 内核入口。
//
// 一个二进制，两种启动结果：
//   - 正常态：加载内核 → 拉起服务 → self-test → 对外服务
//   - 救援态：只挂 Web 外壳 + 内核日志 + 救援界面（服务全部不加载）
//
// 刻意不 import 任何领域包：内核不知道"更新"为何物。
// 更新能力属于 services/deployer（用户态服务），通过 syscall 调用内核。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"samryetha/kernel/boot"
	"samryetha/kernel/events"
	"samryetha/kernel/perm"
	"samryetha/kernel/proc"
	"samryetha/kernel/store"
	"samryetha/kernel/syscall"
	"samryetha/kernel/task"
	"samryetha/kernel/ui"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// kernelLog 是内核日志环（有界）。服务日志另存于 store，两者分离（§8）。
type kernelLog struct {
	mu    sync.RWMutex
	lines []kvLine
	max   int
}

type kvLine struct {
	TS    int64          `json:"ts"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Meta  map[string]any `json:"meta,omitempty"`
}

func newKernelLog(max int) *kernelLog { return &kernelLog{max: max} }

func (k *kernelLog) add(level, msg string, meta map[string]any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lines = append(k.lines, kvLine{TS: time.Now().UnixMilli(), Level: level, Msg: msg, Meta: meta})
	if len(k.lines) > k.max {
		k.lines = k.lines[len(k.lines)-k.max:]
	}
}

func (k *kernelLog) tail(n int) []kvLine {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if n <= 0 || n > len(k.lines) {
		n = len(k.lines)
	}
	out := make([]kvLine, n)
	copy(out, k.lines[len(k.lines)-n:])
	return out
}

func main() {
	var (
		root   = flag.String("root", env("KERNEL_ROOT", "/opt/samryetha"), "配置与状态根目录")
		addr   = flag.String("listen", env("KERNEL_LISTEN", "127.0.0.1:3030"), "监听地址")
		rescue = flag.Bool("rescue", false, "强制以救援模式启动（不加载服务）")
	)
	flag.Parse()

	dataDir := filepath.Join(*root, "var")
	opts := boot.Options{Root: *root, DataDir: dataDir, ForceRescue: *rescue}

	klog := newKernelLog(4000)
	bus := events.New(4096, nil)
	policy := loadPolicy(*root)
	authMode := loadAuthMode(*root)
	procs := proc.NewManager(func(pid int, stream, line string) {
		klog.add("debug", line, map[string]any{"pid": pid, "stream": stream})
	})
	tasks := task.NewScheduler(procs, 1, func(taskID, level, msg string) {
		klog.add(level, msg, map[string]any{"task": taskID})
	})
	st, err := store.Open(filepath.Join(dataDir, "store"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// 启动序列。每个 step 是内核职责，不含领域语义。
	outcome, bootErr := boot.Boot(opts, []func(boot.Phase) error{
		func(p boot.Phase) error {
			// 阶段：配置。救援模式的关键触发点之一——配置非法即进救援。
			if err := policy.Validate(); err != nil {
				return err
			}
			bus.Emit("kernel.boot.config_ok", "kernel", nil, map[string]any{"root": *root})
			return nil
		},
		func(p boot.Phase) error {
			// 阶段：self-test。内核要用到的基础设施是否可用。
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return fmt.Errorf("state dir unwritable: %w", err)
			}
			// 存储自检：写入并读回一个探针键
			if err := st.Set("kernel", "selftest", "ok"); err != nil {
				return fmt.Errorf("store selftest: %w", err)
			}
			if _, _, err := st.Get("kernel", "selftest"); err != nil {
				return fmt.Errorf("store readback: %w", err)
			}
			bus.Emit("kernel.boot.selftest_ok", "kernel", nil, nil)
			return nil
		},
		func(p boot.Phase) error {
			// 阶段：计划。此处只登记 syscall 表可用性（插件加载在 P3 接入）。
			bus.Emit("kernel.boot.plan_ok", "kernel", nil, map[string]any{
				"syscall_version": syscall.Version,
			})
			return nil
		},
	})
	if bootErr != nil {
		log.Fatalf("kernel boot error: %v", bootErr)
	}

	shell := ui.NewShell()
	table := syscall.Register(syscall.Deps{
		Bus: bus, Procs: procs, Tasks: tasks, Store: st, Policy: policy, UI: shell,
		Log: func(level, msg string, fields map[string]any) { klog.add(level, msg, fields) },
	})

	mux := http.NewServeMux()

	// ---- 内核自身的最小 API ----
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// 前端外壳：只渲染布局与插槽，业务内容来自各服务的 ui.declare
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		meta := map[string]any{"syscallVersion": syscall.Version,
			"note": "kernel has no domain knowledge; services provide features"}
		data := ui.RenderData{
			Title: "Samryetha control", Subtitle: "kernel + services",
			Nav: shell.Nav(), Sources: shell.Sources(), KernelMeta: meta,
		}
		if !outcome.Ready {
			data.Rescue = &ui.RescueInfo{Reason: outcome.Reason, Phase: string(outcome.Phase), Notes: outcome.Notes}
		}
		html, err := shell.Render(data)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	})
	mux.HandleFunc("/api/kernel/meta", func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 0, len(table))
		for n := range table {
			names = append(names, n)
		}
		writeJSON(w, 200, map[string]any{
			"syscallVersion": syscall.Version,
			"syscalls":       names,
			"startedAt":      outcome.StartedAt.Format(time.RFC3339),
			"ready":          outcome.Ready,
			"roles":          perm.Roles(),
			"note":           "kernel has no domain knowledge; services provide features",
		})
	})
	mux.HandleFunc("/api/kernel/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"items": bus.History(r.URL.Query().Get("topic"), 0, 300)})
	})
	mux.HandleFunc("/api/kernel/log", func(w http.ResponseWriter, r *http.Request) {
		items := klog.tail(500)
		if src := r.URL.Query().Get("source"); src != "" {
			filtered := make([]kvLine, 0, len(items))
			for _, it := range items {
				if it.Meta == nil {
					continue
				}
				if p, _ := it.Meta["plugin"].(string); p == src {
					filtered = append(filtered, it)
				}
			}
			items = filtered
		}
		writeJSON(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("/api/kernel/procs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"procs": procs.List()})
	})
	mux.HandleFunc("/api/kernel/tasks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"tasks": tasks.List()})
	})
	mux.HandleFunc("/api/kernel/rescue", func(w http.ResponseWriter, r *http.Request) {
		if outcome.Ready {
			writeJSON(w, 404, map[string]any{"error": "not in rescue mode"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"reason": outcome.Reason, "phase": outcome.Phase,
			"notes": outcome.Notes, "startedAt": outcome.StartedAt.Format(time.RFC3339),
			"logTail": klog.tail(50),
		})
	})

	// ---- syscall 统一入口（外部插件 / CLI 走这里；内建插件直接调 Go 接口） ----
	mux.HandleFunc("/api/kernel/call", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]any{"error": "POST required"})
			return
		}
		var req struct {
			Name   string         `json:"name"`
			Args   map[string]any `json:"args"`
			Plugin string         `json:"plugin"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": map[string]any{"code": "bad_json", "message": err.Error()}})
			return
		}
		h, ok := table[req.Name]
		if !ok {
			writeJSON(w, 404, map[string]any{"ok": false, "error": map[string]any{"code": "unknown_call", "message": req.Name}})
			return
		}
		// 调用方身份来自认证中间件（P4 接入 OIDC）；此处从请求头取，便于本地验证。
		// 调用方身份：生产由认证驱动（P4 接 OIDC）注入可信主体。
		// 安全约束：**不采信请求头里的角色**——否则任何调用方都能自称 admin。
		// 角色一律由策略按 subject 决定（perm.Policy.RoleOf）。
		caller := syscall.CallerInfo{
			Plugin:  strings.TrimSpace(firstNonEmpty(req.Plugin, r.Header.Get("X-Kernel-Plugin"), "external")),
			Subject: trustedSubject(r, authMode),
			Roles:   nil,
		}
		data, cerr := h(r.Context(), syscall.Call{Name: req.Name, Args: req.Args, Caller: caller})
		if cerr != nil {
			code := 400
			if cerr.Code == "denied" {
				code = 403
			}
			writeJSON(w, code, map[string]any{"ok": false, "error": map[string]any{"code": cerr.Code, "message": cerr.Message}})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "data": data})
	})

	if !outcome.Ready {
		log.Printf("KERNEL IN RESCUE MODE: %s", outcome.Reason)
	} else {
		log.Printf("kernel ready (syscall %s, %d calls)", syscall.Version, len(table))
	}
	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// trustedSubject 解析调用方身份。
//
// 身份来源由配置决定（etc/auth.txt 的 mode），而不是散落的开关：
//
//	proxy    受信反代/认证中间件注入 X-Kernel-Subject（生产推荐）
//	header   直接接受 X-Kernel-Subject（仅供本机只读调试，须绑 127.0.0.1）
//	none     无身份（一律 viewer，最小权限）
//
// 关键安全约束：**角色永远不来自请求**，只由策略按 subject 决定。
// 认证（证明"你是这个 sub"）在 mode=proxy 时由前置中间件负责；
// 内核只消费它放好的可信标头。
func trustedSubject(r *http.Request, mode string) string {
	switch mode {
	case "header":
		return strings.TrimSpace(r.Header.Get("X-Kernel-Subject"))
	case "proxy":
		return strings.TrimSpace(r.Header.Get("X-Kernel-Authenticated-Subject"))
	default:
		return ""
	}
}

// loadAuthMode 从 etc/auth.txt 读取身份来源模式，默认 none（最小权限）。
func loadAuthMode(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "etc", "auth.txt"))
	if err != nil {
		return "none"
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "mode") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return "none"
}

// loadPolicy 从 etc/policy.yaml 的同构 JSON（或环境变量）加载角色映射。
// 为保持内核零依赖，这里用最简单的 "subject=role" 行式配置。
func loadPolicy(root string) *perm.Policy {
	p := perm.DefaultPolicy()
	// 默认最小权限；显式开启内网模式才放宽
	if os.Getenv("KERNEL_DEFAULT_ROLE") != "" {
		p.Default = perm.Role(os.Getenv("KERNEL_DEFAULT_ROLE"))
	}
	path := filepath.Join(root, "etc", "policy.txt")
	b, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		left := strings.TrimSpace(parts[0])
		right := strings.TrimSpace(parts[1])
		// "cap:role=capability" 形式：为角色追加**业务**能力点（内核不内置领域词）
		if strings.HasPrefix(left, "cap:") {
			role := perm.Role(strings.TrimPrefix(left, "cap:"))
			p.Extra[role] = append(p.Extra[role], right)
			continue
		}
		p.Assign[left] = perm.Role(right)
	}
	return p
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
