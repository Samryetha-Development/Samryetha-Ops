// Package syscall 定义内核与插件之间的唯一契约。
//
// 设计要点（见 docs/architecture.md §4）：
//   - 插件只能通过这些调用与内核交互，不得依赖内核内部结构。
//   - 内建插件走 Go 接口（零序列化）；外部插件走 JSON-RPC over stdio/UDS。
//     两者暴露同一组调用名与语义，因此同一份插件逻辑可内建或外置。
//   - 表只增不改：同大版本内不得删除或改变已有调用的语义。
package syscall

import "context"

// Version 是当前 syscall 表版本。插件在 manifest 里声明它依赖的版本。
const Version = "kernel/v1"

// Envelope 是所有调用的统一返回信封。
type Envelope struct {
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data,omitempty"`
	Error *CallError     `json:"error,omitempty"`
}

// CallError 描述一次调用失败。
type CallError struct {
	Code    string `json:"code"`    // 机器可读：invalid_args / denied / not_found / internal …
	Message string `json:"message"` // 人类可读
}

func (e *CallError) Error() string { return e.Code + ": " + e.Message }

// Call 是一次 syscall 请求。
type Call struct {
	Name   string         `json:"name"`   // 形如 "proc.spawn"
	Args   map[string]any `json:"args"`   // 调用参数
	Caller CallerInfo     `json:"caller"` // 由内核填充，插件不可伪造
}

// CallerInfo 标识调用方，供权限判定与审计使用。
type CallerInfo struct {
	Plugin  string   `json:"plugin"`  // 插件 id，如 "deployer"
	Roles   []string `json:"roles"`   // 主体角色
	Subject string   `json:"subject"` // 身份标识（如 OIDC sub）
}

// Handler 实现单个 syscall。内核为每个名字注册一个 Handler。
type Handler func(ctx context.Context, call Call) (map[string]any, *CallError)

// Table 是所有已注册 syscall 的名字 → 实现。内建与外部后端共用它做权限校验与派发。
type Table map[string]Handler

// Permission 是一个能力点，形如 "proc.manage"。
type Permission string

// 权限点清单。内核在此集中声明哪些调用需要什么权限，避免散落在各处。
var CallPermissions = map[string]Permission{
	"log.write":       "", // 任何插件都可写自己的日志
	"log.query":       "log.read",
	"event.emit":      "event.emit",
	"event.subscribe": "event.read",
	"event.history":   "event.read",
	"store.get":       "store.own",
	"store.set":       "store.own",
	"store.del":       "store.own",
	"store.list":      "store.own",
	"fs.read":         "fs.read",
	"fs.write":        "fs.write",
	"fs.list":         "fs.read",
	"config.get":      "config.read",
	"config.set":      "config.write",
	"config.list":     "config.read",
	"config.watch":    "config.read",
	"auth.public":     "", // 取当前主体的公开信息
	"auth.check":      "", // 自查能力点
	"proc.spawn":      "proc.manage",
	"proc.signal":     "proc.manage",
	"proc.wait":       "proc.manage",
	"proc.list":       "proc.read",
	"proc.output":     "proc.read",
	"task.submit":     "task.submit",
	"task.status":     "task.read",
	"task.list":       "task.read",
	"task.cancel":     "task.submit",
	"task.once":       "task.submit",
	"task.cron":       "task.submit",
	"route.mount":     "route.mount",
	"route.unmount":   "route.mount",
	"ws.mount":        "route.mount",
	"ui.declare":      "", // 只声明 UI，不改内核状态
	"ui.withdraw":     "", // 撤销声明（服务停止时）
	"time.now":        "",
	"rand.bytes":      "",
	"hash.sha256":     "",
}
