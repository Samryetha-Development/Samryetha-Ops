package main

import (
	"context"
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

	"samryetha/sdk"
	"samryetha/services/deploycfg"
	"samryetha/services/deployer"
	drivers "samryetha/services/drivers"
	gitdriver "samryetha/services/drivers/git"
	healthdriver "samryetha/services/drivers/health"
	processdriver "samryetha/services/drivers/process"
	"samryetha/services/statuspage"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// kernelLog 是内核日志环（有界）。服务日志另存于 store，两者分离。
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

	shell := ui.NewShell()
	wire, werr := buildWiring(*root)
	if werr != nil {
		log.Fatalf("wiring: %v", werr)
	}

	outcome, bootErr := boot.Boot(opts, []func(boot.Phase) error{
		func(p boot.Phase) error {
			if err := policy.Validate(); err != nil {
				return err
			}
			bus.Emit("kernel.boot.config_ok", "kernel", nil, map[string]any{"root": *root})
			return nil
		},
		func(p boot.Phase) error {
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return fmt.Errorf("state dir unwritable: %w", err)
			}
			if err := st.Set("kernel", "selftest", "ok"); err != nil {
				return fmt.Errorf("store selftest: %w", err)
			}
			bus.Emit("kernel.boot.selftest_ok", "kernel", nil, nil)
			return nil
		},
		func(p boot.Phase) error {
			bus.Emit("kernel.boot.plan_ok", "kernel", nil, map[string]any{"syscall_version": syscall.Version})
			return nil
		},
	})
	if bootErr != nil {
		log.Fatalf("kernel boot error: %v", bootErr)
	}

	table := syscall.Register(syscall.Deps{
		Bus: bus, Procs: procs, Tasks: tasks, Store: st, Policy: policy, UI: shell,
		Config: wire.Config, FS: wire.FS, Routes: wire.Routes, Cron: wire.Cron, Root: *root,
		Log: func(level, msg string, fields map[string]any) { klog.add(level, msg, fields) },
	})

	// --- 服务加载（仅正常态；救援模式不加载任何服务）---
	services := []string{}
	if outcome.Ready {
		loaded, err := startServices(*root, table, bus, klog, wire)
		if err != nil {
			log.Printf("services: %v", err)
		}
		services = loaded
		bus.Emit("kernel.services.started", "kernel", nil, map[string]any{"services": loaded})
	}

	mux := http.NewServeMux()
	registerKernelAPI(mux, klog, bus, table, shell, wire, procs, tasks, policy, outcome, services)
	registerSyscallEntry(mux, table, authMode)

	if !outcome.Ready {
		log.Printf("KERNEL IN RESCUE MODE: %s", outcome.Reason)
	} else {
		log.Printf("kernel ready (syscall %s, %d calls, %d services)", syscall.Version, len(table), len(services))
	}
	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, wire.serveRouted(mux)); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// startServices 从 deploy.yaml 装载服务：注册驱动、启动 statuspage、注册部署定时任务。
func startServices(root string, table syscall.Table, bus *events.Bus, klog *kernelLog, wire *wiring) ([]string, error) {
	cfgPath := filepath.Join(root, "etc", "deploy.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		return nil, fmt.Errorf("no deploy.yaml at %s", cfgPath)
	}
	cfg, err := deploycfg.Load(cfgPath)
	if err != nil {
		return nil, err
	}

	// syscall 侧适配器：把内核表暴露成 sdk.Kernel（内建服务走这条路）
	k := newSyscallAdapter(table, "kernel")
	k.cron = wire.Cron // 让内建服务能把动作绑定到定时任务

	// 驱动注册
	reg := drivers.NewRegistry()
	reg.AddSource(gitdriver.New(k))
	reg.AddHealth(healthdriver.NewHTTP(k))
	reg.AddHealth(healthdriver.NewTCP(k))
	reg.AddHealth(healthdriver.NewExec(k))

	var loaded []string

	// deployer：为每个 target 注册定时部署任务
	dep := deployer.New(k, reg)
	for _, t := range cfg.Targets {
		// 进程驱动按配置实例化
		switch t.ProcessDriver {
		case "pm2":
			reg.AddProcesses(processdriver.NewPM2(k, t.ProcessNames))
		case "systemd":
			reg.AddProcesses(processdriver.NewSystemd(k, t.ProcessNames))
		case "exec":
			reg.AddProcesses(processdriver.NewExec(k, t.Restart))
		}
		// 调度
		for _, sc := range cfg.Schedule {
			if sc.Target != t.ID || !sc.Enabled {
				continue
			}
			plan := t
			job := sc
			_, err := sdk.Cron(k, job.Cron, "deploy:"+t.ID, []sdk.Step{{
				Name: "deploy." + t.ID, Argv: []string{"true"},
			}})
			if err != nil {
				klog.add("warn", "cron register failed for "+t.ID+": "+err.Error(), nil)
				continue
			}
			// 真实部署由内核 cron 触发 → 调用 deployer.Deploy
			wire.Cron.SetHook("deploy:"+t.ID, func() {
				out := dep.Deploy(context.Background(), plan)
				klog.add("info", fmt.Sprintf("scheduled deploy %s → %s", out.Target, out.State), nil)
			})
			klog.add("info", fmt.Sprintf("scheduled %s (%s)", t.ID, job.Cron), nil)
		}
	}
	loaded = append(loaded, "deployer")

	// statuspage：按 target 生成观测目标
	if containsStr(cfg.Plugins["services"], "statuspage") {
		var targets []statuspage.Target
		for _, t := range cfg.Targets {
			name := t.ID
			tg := statuspage.Target{ID: t.ID, Name: name}
			if len(t.Health) > 0 {
				tg.URL = t.Health[0].URL
				tg.TCP = t.Health[0].Addr
				tg.Cmd = t.Health[0].Command
			}
			targets = append(targets, tg)
		}
		// 输出目录与生成方式来自配置（不再硬编码）：
		// Caddy 指向哪个目录、页面由谁渲染，都是部署决策而非代码常量。
		out := "status:www/index.html"
		var generator []string
		sched := "*/1 * * * *"
		if v, ok := wire.Config.Get("statuspage.output"); ok {
			if str, ok := v.(string); ok && str != "" {
				out = str
			}
		}
		if v, ok := wire.Config.Get("statuspage.generator.command"); ok {
			if cmd, ok := v.(string); ok && cmd != "" {
				generator = []string{"sh", "-lc", cmd}
			}
		}
		if v, ok := wire.Config.Get("statuspage.schedule"); ok {
			if str, ok := v.(string); ok && str != "" {
				sched = str
			}
		}
		sp := &statuspage.Service{K: k, Reg: reg, Targets: targets,
			Output: out, Generator: generator, Schedule: sched}
		if err := sp.Start(context.Background()); err != nil {
			klog.add("warn", "statuspage start failed: "+err.Error(), nil)
		} else {
			loaded = append(loaded, "statuspage")
		}
	}

	bus.Emit("kernel.services.loaded", "kernel", nil, map[string]any{"services": loaded})
	return loaded, nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func registerKernelAPI(mux *http.ServeMux, klog *kernelLog, bus *events.Bus, table syscall.Table,
	shell *ui.Shell, wire *wiring, procs *proc.Manager, tasks *task.Scheduler,
	policy *perm.Policy, outcome boot.Outcome, services []string) {

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
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
			"syscallVersion": syscall.Version, "syscalls": names, "roles": perm.Roles(),
			"ready": outcome.Ready, "services": services,
			"startedAt": outcome.StartedAt.Format(time.RFC3339),
			"note":      "kernel has no domain knowledge; services provide features",
		})
	})
	mux.HandleFunc("/api/kernel/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"items": bus.History(r.URL.Query().Get("topic"), 0, 300)})
	})
	mux.HandleFunc("/api/kernel/log", func(w http.ResponseWriter, r *http.Request) {
		items := klog.tail(500)
		if src := r.URL.Query().Get("source"); src != "" {
			filtered := make([]kvLine, 0)
			for _, it := range items {
				if it.Meta != nil {
					if p, _ := it.Meta["plugin"].(string); p == src {
						filtered = append(filtered, it)
					}
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
	mux.HandleFunc("/api/kernel/routes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"routes": wire.Routes.Mounts()})
	})
	mux.HandleFunc("/api/kernel/scopes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"scopes": wire.FS.Scopes()})
	})
	mux.HandleFunc("/api/kernel/cron", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"jobs": wire.Cron.List()})
	})
	mux.HandleFunc("/api/kernel/rescue", func(w http.ResponseWriter, r *http.Request) {
		if outcome.Ready {
			writeJSON(w, 404, map[string]any{"error": "not in rescue mode"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"reason": outcome.Reason, "phase": outcome.Phase, "notes": outcome.Notes,
			"startedAt": outcome.StartedAt.Format(time.RFC3339), "logTail": klog.tail(50),
		})
	})
	_ = policy
}

func registerSyscallEntry(mux *http.ServeMux, table syscall.Table, authMode string) {
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

// trustedSubject 解析调用方身份。角色永远不来自请求，只由策略按 subject 决定。
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

func loadPolicy(root string) *perm.Policy {
	p := perm.DefaultPolicy()
	// 内建服务显式授权为 operator：权限仍走同一条判定路径，
	// 只是在策略里登记，避免"内建即绕过"的隐性特权。
	p.Assign[BuiltinSubject] = perm.RoleOperator
	if os.Getenv("KERNEL_DEFAULT_ROLE") != "" {
		p.Default = perm.Role(os.Getenv("KERNEL_DEFAULT_ROLE"))
	}
	b, err := os.ReadFile(filepath.Join(root, "etc", "policy.txt"))
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
		left, right := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if strings.HasPrefix(left, "cap:") {
			p.Extra[perm.Role(strings.TrimPrefix(left, "cap:"))] = append(
				p.Extra[perm.Role(strings.TrimPrefix(left, "cap:"))], right)
			continue
		}
		p.Assign[left] = perm.Role(right)
	}
	return p
}
