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

	"samryetha/kernel/auth"
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
	migrationsdriver "samryetha/services/drivers/migrations"
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

// sourceOf 判定一条日志的来源：服务经 syscall 写日志时内核会带上 plugin 字段，
// 没带的（内核自己写的）归为 "kernel"。
//
// 为什么必须给默认值：内核自身的日志 Meta 是空的，于是按来源筛选时
// "kernel" 一条也匹配不到——控制台的"日志 → kernel"永远空白，
// 看上去像内核从不写日志，实际是筛选口径漏了默认来源。
func sourceOf(it kvLine) string {
	if it.Meta != nil {
		if p, _ := it.Meta["plugin"].(string); p != "" {
			return p
		}
	}
	return "kernel"
}

// asNumber 把配置里的数值统一成 float64。
// 为什么需要：配置有两个来源，数字类型并不一致——deploy.yaml 走自带解析器，
// 整数是 int；JSON 配置解出来是 float64。只断言其中一种，会让另一种来源里
// 写下的配置**静默失效**（界面看不出任何异常，只是行为永远停在默认值）。
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// resolveSchedule 把"描述文件里的默认调度"与"配置树里的覆盖"合成最终值。
//
// get 就是 config.Tree.Get：控制台的设置表单把值写到 schedule.<id> / enabled.<id>，
// 所以这两个键必须在这里被**真正读取**——否则表单就是摆设（改完什么都不发生）。
// 返回的 bool 是"最终是否自动调度"：开关为关、或 cron 为空，都不调度。
func resolveSchedule(def deploycfg.Schedule, get func(string) (any, bool)) (string, bool) {
	cron := strings.TrimSpace(def.Cron)
	enabled := def.Enabled
	if v, ok := get("schedule." + def.Target); ok {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			cron = strings.TrimSpace(s)
		}
	}
	if v, ok := get("enabled." + def.Target); ok {
		enabled = truthy(v)
	}
	return cron, enabled && cron != ""
}

// truthy 解析布尔配置：控制台的表单存的是字符串 "true"/"false"，
// 而 JSON 配置里可能是 bool。只认一种会让开关静默失效。
func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		s := strings.TrimSpace(strings.ToLower(b))
		return s == "true" || s == "1" || s == "on" || s == "yes"
	}
	return false
}

// 包级引用：renderShell 需要 shell/outcome，但它们是 main 的局部变量。
// 显式提升为包级，避免给每个 handler 都传一遍。
var (
	shellRef   *ui.Shell
	outcomeRef boot.Outcome
)

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

	// 认证：把配置解析成驱动。配置错误直接拒绝启动——
	// 曾因 auth 配置被忽略（mode=header 却暴露公网）导致任何人都能自称管理员。
	authCfg, err := auth.Load(*root)
	if err != nil {
		log.Fatalf("auth config: %v", err)
	}
	// admin_subs / admin_groups 是便捷项：policy 里已有的映射优先
	// （单一事实来源仍是 policy.txt）。
	for _, sub := range authCfg.AdminSubs {
		if s := strings.TrimSpace(sub); s != "" {
			if _, ok := policy.Assign[s]; !ok {
				policy.Assign[s] = perm.RoleAdmin
			}
		}
	}
	for _, g := range authCfg.AdminGroups {
		if s := strings.TrimSpace(g); s != "" {
			if _, ok := policy.AssignGroups[s]; !ok {
				policy.AssignGroups[s] = perm.RoleAdmin
			}
		}
	}
	authDriver, err := auth.New(authCfg, *addr, func(f string, a ...any) {
		klog.add("info", fmt.Sprintf(f, a...), nil)
	})
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	log.Printf("auth mode: %s", authCfg.Mode)

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
	shellRef = shell
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
	outcomeRef = outcome

	table := syscall.Register(syscall.Deps{
		Bus: bus, Procs: procs, Tasks: tasks, Store: st, Policy: policy, UI: shell,
		Config: wire.Config, FS: wire.FS, Routes: wire.Routes, Cron: wire.Cron,
		Actions: wire.Actions, Root: *root,
		Log: func(level, msg string, fields map[string]any) { klog.add(level, msg, fields) },
		// LogQuery 此前只声明、从未赋值：log.query 于是永远返回空列表——
		// 调用方看到的是"没有日志"，而不是"这个调用没人实现"。这里补上真实实现，
		// 与 /api/kernel/log 的口径保持一致（同为来源筛选 + 子串匹配）。
		LogQuery: func(source, q string, limit int) []any {
			if limit <= 0 {
				limit = 200
			}
			items := klog.tail(0)
			out := make([]any, 0, len(items))
			for _, it := range items {
				if source != "" && sourceOf(it) != source {
					continue
				}
				if q != "" && !strings.Contains(it.Msg, q) {
					continue
				}
				out = append(out, map[string]any{
					"ts": it.TS, "level": it.Level, "msg": it.Msg, "source": sourceOf(it),
				})
			}
			if len(out) > limit {
				out = out[len(out)-limit:]
			}
			return out
		},
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
	// 认证驱动的自有端点（登录/回调/登出）先注册：
	// 它们的具体路径必须能盖过 /update/ 这类前缀处理器。
	authDriver.Mount(mux)
	registerKernelAPI(mux, klog, bus, table, shell, wire, procs, tasks, policy, outcome, services)
	registerSyscallEntry(mux, table)

	// 认证闸门：除公开路径外，一切请求都必须先解析出主体。
	// 公开路径 = /healthz（探活）+ 驱动自有端点（登录/回调/登出）。
	publicPaths := append([]string{"/healthz"}, authDriver.PublicPaths()...)
	gate := func(h http.Handler) http.Handler { return auth.Gate(authDriver, publicPaths, h) }

	if !outcome.Ready {
		log.Printf("KERNEL IN RESCUE MODE: %s", outcome.Reason)
	} else {
		log.Printf("kernel ready (syscall %s, %d calls, %d services)", syscall.Version, len(table), len(services))
	}
	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, wire.serveRouted(mux, gate)); err != nil {
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

	// syscall 侧适配器：把内核表暴露成 sdk.Kernel（内建服务走这条路）。
	//
	// 每个内建服务必须有自己的适配器（独立的 plugin 名）：
	// 共用同一个会让两个服务的 UI 声明互相覆盖（都叫 "kernel"），
	// 表现为"只看到最后一个服务的面板"——排查时极难定位。
	mkAdapter := func(name string) *syscallAdapter {
		a := newSyscallAdapter(table, name)
		a.cron = wire.Cron
		a.actions = wire.Actions
		return a
	}
	k := mkAdapter("kernel")
	_ = k

	// 驱动注册
	reg := drivers.NewRegistry()
	reg.AddSource(gitdriver.New(k))
	reg.AddHealth(healthdriver.NewHTTP(k))
	reg.AddHealth(healthdriver.NewTCP(k))
	reg.AddHealth(healthdriver.NewExec(k))
	// 迁移驱动：此前从未注册，导致 deploy.yaml 里的 migrations 配置静默失效。
	reg.AddMigrations(migrationsdriver.New(k))

	var loaded []string

	// deployer：为每个 target 注册定时部署任务
	dep := deployer.New(mkAdapter("deployer"), reg)

	// 调度与"自动部署"开关的**最终值** = deploy.yaml 的默认值 + 配置树的覆盖。
	//
	// 配置树那一层就是控制台的设置表单（config.set → var/config.json，重启仍在）。
	// 只认 deploy.yaml 的话，那个表单就是"改了没用"的摆设——本项目反复踩过的病。
	scheduleOf := func(id string) (string, bool) {
		def := deploycfg.Schedule{Target: id}
		for _, sc := range cfg.Schedule {
			if sc.Target == id {
				def = sc
				break
			}
		}
		return resolveSchedule(def, wire.Config.Get)
	}

	var (
		allPlans    []deployer.Plan
		plansByID   = map[string]deployer.Plan{}
		scheduleMap = map[string]string{}
		jobIDs      = map[string]string{}
	)

	// scheduleTarget 按当前生效的 cron 注册任务；cron 非法或开关为关则不动。
	scheduleTarget := func(id string) {
		cron, on := scheduleOf(id)
		if !on {
			return
		}
		jobID, err := sdk.Cron(k, cron, "deploy:"+id, []sdk.Step{{
			Name: "deploy." + id, Argv: []string{"true"},
		}})
		if err != nil {
			klog.add("warn", "cron register failed for "+id+": "+err.Error()+"（检查控制台里的 cron 表达式）", nil)
			return
		}
		plan := plansByID[id]
		wire.Cron.SetHook("deploy:"+id, func() {
			out := dep.Deploy(context.Background(), plan)
			klog.add("info", fmt.Sprintf("scheduled deploy %s → %s", out.Target, out.State), nil)
		})
		jobIDs[id] = jobID
		klog.add("info", fmt.Sprintf("scheduled %s (%s)", id, cron), nil)
	}
	unscheduleTarget := func(id string) {
		if jobID, ok := jobIDs[id]; ok {
			wire.Cron.Remove(jobID)
			delete(jobIDs, id)
			klog.add("info", "unscheduled "+id+" (自动部署已关闭)", nil)
		}
	}

	for _, t := range cfg.Targets {
		// 目标级开关必须真正生效：此前 deploycfg 只对 schedule 读 enabled，
		// target 上的 enabled 被静默忽略（写了 false 也照跑）。停用的目标不注册
		// 调度、不出现在控制台，这样"停用"才是一个可信的状态。
		if !t.Enabled {
			klog.add("info", "target "+t.ID+" is disabled; not scheduling it", nil)
			continue
		}
		// 进程驱动按配置实例化
		switch t.ProcessDriver {
		case "pm2":
			reg.AddProcesses(processdriver.NewPM2(k, t.ProcessNames))
		case "systemd":
			reg.AddProcesses(processdriver.NewSystemd(k, t.ProcessNames))
		case "exec":
			reg.AddProcesses(processdriver.NewExec(k, t.Restart))
		}
		// 生效值写进计划：控制台用它回显"自动部署"开关。
		cron, on := scheduleOf(t.ID)
		t.AutoDeploy = on
		allPlans = append(allPlans, t)
		plansByID[t.ID] = t
		// 即使开关是关的也带上 cron：否则设置表单会整块消失，用户再也开不回来。
		if strings.TrimSpace(cron) != "" {
			scheduleMap[t.ID] = cron
		}
	}

	// 注册调度。每个目标的 job 名字固定为 deploy:<id>，以便配置变化时热替换。
	for _, p := range allPlans {
		scheduleTarget(p.ID)
	}

	// 控制台改了调度 → 立即重算并重声明面板，不必等 ui_refresh 或重启内核。
	//
	// 只认 deploy.yaml 的写法会让设置表单变成"改了没用"的摆设；这里让配置树的
	// 覆盖值**真正生效**：改 cron 重新注册任务，改开关则注册或撤销。
	unwatch := wire.Config.Watch(func(path string) {
		id := ""
		switch {
		case strings.HasPrefix(path, "schedule."):
			id = strings.TrimPrefix(path, "schedule.")
		case strings.HasPrefix(path, "enabled."):
			id = strings.TrimPrefix(path, "enabled.")
		default:
			return
		}
		if _, ok := plansByID[id]; !ok {
			return
		}
		cron, on := scheduleOf(id)
		unscheduleTarget(id)
		if on {
			scheduleTarget(id)
		}
		// UI 读的是 plansByID 与 scheduleMap，两个都要更新（值类型要回写切片元素）。
		if p, ok := plansByID[id]; ok {
			p.AutoDeploy = on
			plansByID[id] = p
			for i := range allPlans {
				if allPlans[i].ID == id {
					allPlans[i].AutoDeploy = on
				}
			}
		}
		if strings.TrimSpace(cron) != "" {
			scheduleMap[id] = cron
		}
		klog.add("info", fmt.Sprintf("schedule for %s reloaded from config (cron=%q on=%v)", id, cron, on), nil)
		// 后台重声明：别让保存请求等着一整套面板采集（进程/磁盘/提交）跑完。
		go func() { _ = dep.DeclareFullUI(context.Background(), allPlans, scheduleMap) }()
	})
	_ = unwatch // 常驻进程生命周期，不需要注销

	// 收集全部计划，供动作注册与 UI 声明使用。
	// 停用的目标同样排除：否则控制台会出现"点了必然失败"的按钮，比没有按钮更误导。
	if err := dep.RegisterActions(allPlans); err != nil {
		log.Printf("deployer action register failed: %v", err)
		klog.add("warn", "deployer action register failed: "+err.Error(), nil)
	}
	if err := dep.DeclareFullUI(context.Background(), allPlans, scheduleMap); err != nil {
		log.Printf("deployer ui declare failed: %v", err)
		klog.add("warn", "deployer ui declare failed: "+err.Error(), nil)
	}
	// 面板数据不能只采一次：否则控制台永远显示开机那一刻的进程/磁盘/提交快照。
	// 间隔可配（deployer.ui_refresh_seconds），默认 60s。
	uiRefresh := time.Minute
	if v, ok := wire.Config.Get("deployer.ui_refresh_seconds"); ok {
		// 只断言 float64 是不够的：deploy.yaml 由自带解析器读入，整数是 int，
		// 于是 `ui_refresh_seconds: 1` 会被静默忽略、永远停在默认间隔。
		// 配置"写了却不生效"是本项目最常见的病，这里按数值统一取值。
		if f, ok := asNumber(v); ok && f > 0 {
			uiRefresh = time.Duration(f * float64(time.Second))
		}
	}
	dep.StartUIRefresh(context.Background(), allPlans, scheduleMap, uiRefresh)
	loaded = append(loaded, "deployer")

	// statuspage：按 target 生成观测目标
	if containsStr(cfg.Plugins["services"], "statuspage") {
		var targets []statuspage.Target
		for _, t := range cfg.Targets {
			// 停用的目标不再纳入观测：否则概览会一直显示它的故障，
			// 而那是"我们主动关掉的"，不是真故障。
			if !t.Enabled {
				continue
			}
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
		sp := &statuspage.Service{K: mkAdapter("statuspage"), Reg: reg, Targets: targets,
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
	// /update：控制台（与 / 同一个外壳，只是路径不同，便于 Caddy 按路径分流）
	mux.HandleFunc("/update", func(w http.ResponseWriter, r *http.Request) { renderShell(w, r, "/update") })
	mux.HandleFunc("/update/", func(w http.ResponseWriter, r *http.Request) { renderShell(w, r, "/update") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		renderShell(w, r, "/")
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
				if sourceOf(it) == src {
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

func registerSyscallEntry(mux *http.ServeMux, table syscall.Table) {
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
		id := auth.IdentityFrom(r.Context())
		caller := syscall.CallerInfo{
			Plugin: strings.TrimSpace(firstNonEmpty(req.Plugin, r.Header.Get("X-Kernel-Plugin"), "external")),
			// 主体与组只能来自认证闸门写入的上下文——**绝不**采信请求头或请求体，
			// 否则任何人都能自带一个 sub/组自称管理员（这正是本层的意义）。
			Subject: id.Subject,
			Groups:  id.Groups,
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

// renderShell 渲染前端外壳（内核只提供布局与插槽，内容来自各服务声明）。
func renderShell(w http.ResponseWriter, r *http.Request, path string) {
	_ = path
	meta := map[string]any{"syscallVersion": syscall.Version,
		"note": "kernel has no domain knowledge; services provide features"}
	data := ui.RenderData{
		Title: "Samryetha control", Subtitle: "kernel + services",
		Nav: shellRef.Nav(), Sources: shellRef.Sources(), KernelMeta: meta,
	}
	if !outcomeRef.Ready {
		data.Rescue = &ui.RescueInfo{Reason: outcomeRef.Reason, Phase: string(outcomeRef.Phase), Notes: outcomeRef.Notes}
	}
	html, err := shellRef.Render(data)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
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
