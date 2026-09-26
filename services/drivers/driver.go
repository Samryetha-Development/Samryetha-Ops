// Package driver 定义驱动的统一契约。
//
// 驱动 = 与外部系统交互的适配器（git/pm2/systemd/http/release…）。
// 它们运行在**用户态**，通过 syscall 调用内核原语，因此内核不需要认识任何外部系统。
//
// 关键设计：驱动只暴露"领域动作"（如 git.Fetch、pm2.Reload），
// 但每个动作内部一律落到内核的 proc/task/store —— 不存在绕过内核的旁路。
package driver

import (
	"context"

	"samryetha/sdk"
)

// Driver 是所有驱动的基接口。
type Driver interface {
	// Name 返回驱动标识（与 deploy.yaml 里写的名字一致）。
	Name() string
	// Capabilities 声明它提供的能力（供插件依赖解析）。
	Capabilities() []string
}

// Context 是驱动执行时拿到的上下文。
type Context struct {
	Ctx    context.Context
	K      sdk.Kernel
	Target TargetRef
	Log    func(level, msg string, fields map[string]any)
	Emit   func(topic string, payload map[string]any)
}

// TargetRef 是驱动看到的"目标"最小视图（避免驱动依赖 deployer 的类型）。
type TargetRef struct {
	ID      string
	WorkDir string
	Branch  string
}

// Result 是驱动动作的返回。
type Result struct {
	OK     bool
	Output string
	Meta   map[string]any
}

// --- 来源驱动 ---

// Source 负责把代码/产物取到工作目录。
type Source interface {
	Driver
	Fetch(cx *Context) error
	// Resolve 返回本次目标版本（commit sha / 版本号 / 产物摘要）。
	Resolve(cx *Context) (string, error)
	// Reset 把工作目录切到指定版本。
	Reset(cx *Context, rev string) error
	// Changed 返回从 from 到 to 之间变更的文件列表（用于判断是否需要迁移/重装依赖）。
	Changed(cx *Context, from, to string) ([]string, error)
}

// --- 进程驱动 ---

// Processes 负责重启/停止/查看被管进程。
type Processes interface {
	Driver
	Reload(cx *Context) error
	Restart(cx *Context) error
	Stop(cx *Context) error
	Status(cx *Context) ([]ProcessStatus, error)
}

// ProcessStatus 是一条进程状态。
type ProcessStatus struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Restarts int    `json:"restarts"`
	MemMB    int    `json:"mem_mb"`
}

// --- 探活驱动 ---

// HealthCheck 负责一次探活。
type HealthCheck interface {
	Driver
	Check(cx *Context, spec HealthSpec) error
}

// HealthSpec 是一条探活规则（与配置对齐）。
type HealthSpec struct {
	Type    string
	URL     string
	Addr    string
	Command string
	Expect  string
	Timeout int
}

// --- 反代驱动 ---

// Proxy 负责把站点配置落到反代（Caddy/nginx/none）。
type Proxy interface {
	Driver
	Apply(cx *Context, site SiteSpec) error
	Validate(cx *Context) error
}

// SiteSpec 是一个站点块。
type SiteSpec struct {
	Host            string
	Upstreams       []string
	Root            string
	SecurityHeaders bool
}

// --- 迁移驱动 ---

// Migrations 负责数据迁移。
type Migrations interface {
	Driver
	// Needed 判断本次是否真的需要迁移。
	Needed(cx *Context, changed []string) bool
	// Backup 迁移前备份（可选，取决于驱动能力）。
	Backup(cx *Context) (string, error)
	// Apply 执行迁移。
	Apply(cx *Context) error
}

// --- 备份驱动 ---

// Backups 负责数据备份/恢复。
type Backups interface {
	Driver
	Create(cx *Context) (string, error)
	List(cx *Context) ([]BackupInfo, error)
	Restore(cx *Context, id string) error
}

// BackupInfo 描述一个备份。
type BackupInfo struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	At    int64  `json:"at"`
}

// Registry 是驱动注册表。
type Registry struct {
	sources    map[string]Source
	processes  map[string]Processes
	health     map[string]HealthCheck
	proxies    map[string]Proxy
	migrations map[string]Migrations
	backups    map[string]Backups
}

func NewRegistry() *Registry {
	return &Registry{
		sources:    map[string]Source{},
		processes:  map[string]Processes{},
		health:     map[string]HealthCheck{},
		proxies:    map[string]Proxy{},
		migrations: map[string]Migrations{},
		backups:    map[string]Backups{},
	}
}

func (r *Registry) AddSource(d Source)         { r.sources[d.Name()] = d }
func (r *Registry) AddProcesses(d Processes)   { r.processes[d.Name()] = d }
func (r *Registry) AddHealth(d HealthCheck)    { r.health[d.Name()] = d }
func (r *Registry) AddProxy(d Proxy)           { r.proxies[d.Name()] = d }
func (r *Registry) AddMigrations(d Migrations) { r.migrations[d.Name()] = d }
func (r *Registry) AddBackups(d Backups)       { r.backups[d.Name()] = d }

func (r *Registry) Source(name string) (Source, bool)       { d, ok := r.sources[name]; return d, ok }
func (r *Registry) Processes(name string) (Processes, bool) { d, ok := r.processes[name]; return d, ok }
func (r *Registry) Health(name string) (HealthCheck, bool)  { d, ok := r.health[name]; return d, ok }
func (r *Registry) Proxy(name string) (Proxy, bool)         { d, ok := r.proxies[name]; return d, ok }
func (r *Registry) Migrations(name string) (Migrations, bool) {
	d, ok := r.migrations[name]
	return d, ok
}
func (r *Registry) Backups(name string) (Backups, bool) { d, ok := r.backups[name]; return d, ok }

// Names 返回各类已注册驱动的名字（供 UI/文档展示）。
func (r *Registry) Names() map[string][]string {
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	src := map[string]bool{}
	for k := range r.sources {
		src[k] = true
	}
	prc := map[string]bool{}
	for k := range r.processes {
		prc[k] = true
	}
	hlt := map[string]bool{}
	for k := range r.health {
		hlt[k] = true
	}
	pxy := map[string]bool{}
	for k := range r.proxies {
		pxy[k] = true
	}
	mig := map[string]bool{}
	for k := range r.migrations {
		mig[k] = true
	}
	bak := map[string]bool{}
	for k := range r.backups {
		bak[k] = true
	}
	return map[string][]string{
		"source": keys(src), "processes": keys(prc), "health": keys(hlt),
		"proxy": keys(pxy), "migrations": keys(mig), "backups": keys(bak),
	}
}
