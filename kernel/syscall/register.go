// Package syscall 的注册实现：把内核各组件的真实能力暴露为 syscall 表。
//
// 这一层是"内核能力的唯一出口"：插件只能经由它触达 proc/task/store/event/perm。
// 权限在此统一校验——不允许任何调用绕过 CallPermissions。
package syscall

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"samryetha/kernel/config"
	"samryetha/kernel/cron"
	"samryetha/kernel/events"
	"samryetha/kernel/fsops"
	"samryetha/kernel/perm"
	"samryetha/kernel/proc"
	"samryetha/kernel/route"
	"samryetha/kernel/store"
	"samryetha/kernel/task"
	"samryetha/kernel/ui"
)

// Deps 是注册 syscall 所需的内核组件。
type Deps struct {
	Bus    *events.Bus
	Procs  *proc.Manager
	Tasks  *task.Scheduler
	Store  *store.Store
	Policy *perm.Policy
	UI     *ui.Shell
	// 扩展组件（可空；空则对应调用报 unavailable，而不是静默失败）
	Config   *config.Tree
	FS       *fsops.FS
	Routes   *route.Table
	Cron     *cron.Scheduler
	LogQuery func(source, q string, limit int) []any
	Root     string
	Log      func(level, msg string, fields map[string]any)
}

// buildComponent 把 JSON 声明转成受限组件（只认白名单字段，防止任意结构穿透）。
func buildComponent(m map[string]any) ui.Component {
	c := ui.Component{
		Kind: str(m["kind"], ""), ID: str(m["id"], ""), Title: str(m["title"], ""),
		Label: str(m["label"], ""), Text: str(m["text"], ""), Value: str(m["value"], ""),
		Tone: str(m["tone"], ""), Order: int(i64(m["order"], 0)),
	}
	if arr, ok := m["items"].([]any); ok {
		for _, x := range arr {
			if im, ok := x.(map[string]any); ok {
				c.Items = append(c.Items, ui.Item{
					Key: str(im["key"], ""), Label: str(im["label"], ""),
					Value: str(im["value"], ""), Text: str(im["text"], ""),
					At: i64(im["at"], 0), Tone: str(im["tone"], ""),
				})
			}
		}
	}
	if arr, ok := m["fields"].([]any); ok {
		for _, x := range arr {
			if fm, ok := x.(map[string]any); ok {
				c.Fields = append(c.Fields, ui.Field{
					Key: str(fm["key"], ""), Label: str(fm["label"], ""),
					Type: str(fm["type"], "text"), Value: str(fm["value"], ""),
					Help: str(fm["help"], ""),
				})
			}
		}
	}
	if arr, ok := m["bars"].([]any); ok {
		for _, x := range arr {
			c.Bars = append(c.Bars, float64(i64(x, 0)))
		}
	}
	c.Confirm = str(m["confirm"], "")
	if am, ok := m["action"].(map[string]any); ok {
		c.Action = &ui.Action{Kind: str(am["kind"], ""), Target: str(am["target"], ""),
			Method: str(am["method"], ""), Confirm: str(am["confirm"], ""), Body: str(am["body"], "")}
		if c.Confirm == "" {
			c.Confirm = c.Action.Confirm
		}
	}
	return c
}

// Register 构建 syscall 表。
func Register(d Deps) Table {
	t := Table{}

	// ---- 日志 ----
	t["log.write"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		level := str(c.Args["level"], "info")
		msg := str(c.Args["msg"], "")
		d.Log(level, msg, map[string]any{"plugin": c.Caller.Plugin})
		return map[string]any{}, nil
	}

	// ---- 事件 ----
	t["event.emit"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		topic := str(c.Args["topic"], "")
		if topic == "" {
			return nil, &CallError{Code: "invalid_args", Message: "topic required"}
		}
		payload, _ := c.Args["payload"].(map[string]any)
		env := d.Bus.Emit(topic, "service:"+c.Caller.Plugin,
			map[string]any{"id": c.Caller.Subject, "roles": c.Caller.Roles}, payload)
		return map[string]any{"id": env.ID}, nil
	}
	t["event.history"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		topic := str(c.Args["topic"], "")
		since := i64(c.Args["since"], 0)
		limit := int(i64(c.Args["limit"], 200))
		return map[string]any{"items": d.Bus.History(topic, since, limit)}, nil
	}

	// ---- 存储 ----
	t["store.get"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		v, ok, err := d.Store.Get(c.Caller.Plugin, str(c.Args["key"], ""))
		if err != nil {
			return nil, &CallError{Code: "internal", Message: err.Error()}
		}
		return map[string]any{"value": v, "found": ok}, nil
	}
	t["store.set"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if err := d.Store.Set(c.Caller.Plugin, str(c.Args["key"], ""), str(c.Args["value"], "")); err != nil {
			return nil, &CallError{Code: "internal", Message: err.Error()}
		}
		return map[string]any{}, nil
	}
	t["store.del"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		_ = d.Store.Del(c.Caller.Plugin, str(c.Args["key"], ""))
		return map[string]any{}, nil
	}
	t["store.list"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		keys, err := d.Store.List(c.Caller.Plugin, str(c.Args["prefix"], ""))
		if err != nil {
			return nil, &CallError{Code: "internal", Message: err.Error()}
		}
		return map[string]any{"keys": keys}, nil
	}

	// ---- 进程 ----
	t["proc.spawn"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		argv := strSlice(c.Args["argv"])
		if len(argv) == 0 {
			return nil, &CallError{Code: "invalid_args", Message: "argv required"}
		}
		env := strMap(c.Args["env"])
		cwd := str(c.Args["cwd"], "")
		var pid int
		var err error
		// capture=true 时累积输出，可用 proc.output 取回（通用能力，非驱动专属）
		if b, _ := c.Args["capture"].(bool); b {
			pid, err = d.Procs.SpawnCapture(argv, env, cwd)
		} else {
			pid, err = d.Procs.Spawn(argv, env, cwd)
		}
		if err != nil {
			return nil, &CallError{Code: "spawn_failed", Message: err.Error()}
		}
		return map[string]any{"pid": pid}, nil
	}
	t["proc.output"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		out, err := d.Procs.Output(int(i64(c.Args["pid"], 0)))
		if err != nil {
			return nil, &CallError{Code: "not_found", Message: err.Error()}
		}
		return map[string]any{"output": out}, nil
	}
	t["proc.signal"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if err := d.Procs.Signal(int(i64(c.Args["pid"], 0)), str(c.Args["signal"], "term")); err != nil {
			return nil, &CallError{Code: "signal_failed", Message: err.Error()}
		}
		return map[string]any{}, nil
	}
	t["proc.wait"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		code, err := d.Procs.Wait(int(i64(c.Args["pid"], 0)), int(i64(c.Args["timeout_ms"], 0)))
		if err != nil {
			return nil, &CallError{Code: "wait_failed", Message: err.Error()}
		}
		return map[string]any{"code": code}, nil
	}
	t["proc.list"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		return map[string]any{"procs": d.Procs.List()}, nil
	}

	// ---- 任务 ----
	t["task.submit"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		steps := parseSteps(c.Args["steps"])
		if len(steps) == 0 {
			return nil, &CallError{Code: "invalid_args", Message: "steps required"}
		}
		id := d.Tasks.Submit(str(c.Args["name"], "task"), steps, int(i64(c.Args["timeout_ms"], 0)))
		return map[string]any{"task_id": id}, nil
	}
	t["task.status"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		st, ok := d.Tasks.Status(str(c.Args["task_id"], ""))
		if !ok {
			return nil, &CallError{Code: "not_found", Message: "unknown task"}
		}
		return map[string]any{"task": st}, nil
	}
	t["task.cancel"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if err := d.Tasks.Cancel(str(c.Args["task_id"], "")); err != nil {
			return nil, &CallError{Code: "not_found", Message: err.Error()}
		}
		return map[string]any{}, nil
	}
	t["task.list"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		return map[string]any{"tasks": d.Tasks.List()}, nil
	}

	// ---- UI 声明（外壳按插槽渲染；插件不注入任意 DOM） ----
	t["ui.declare"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.UI == nil {
			return nil, &CallError{Code: "unavailable", Message: "ui registry not wired"}
		}
		slotsRaw, _ := c.Args["slots"].(map[string]any)
		slots := map[string][]ui.Component{}
		for slot, arr := range slotsRaw {
			list, _ := arr.([]any)
			for _, x := range list {
				if m, ok := x.(map[string]any); ok {
					slots[slot] = append(slots[slot], buildComponent(m))
				}
			}
		}
		var nav []ui.NavItem
		if arr, ok := c.Args["nav"].([]any); ok {
			for _, x := range arr {
				if m, ok := x.(map[string]any); ok {
					nav = append(nav, ui.NavItem{
						ID: str(m["id"], ""), Label: str(m["label"], ""),
						Icon: str(m["icon"], ""), Order: int(i64(m["order"], 0)),
						Href: str(m["href"], ""),
					})
				}
			}
		}
		d.UI.Declare(&ui.Declaration{Source: c.Caller.Plugin, Slots: slots, Nav: nav})
		return map[string]any{}, nil
	}
	t["ui.withdraw"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.UI != nil {
			d.UI.Withdraw(c.Caller.Plugin)
		}
		return map[string]any{}, nil
	}

	// ---- 杂项（内核提供，避免插件各自造不安全实现） ----
	t["time.now"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		return map[string]any{"ms": time.Now().UnixMilli()}, nil
	}
	t["rand.bytes"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		n := int(i64(c.Args["n"], 16))
		if n <= 0 || n > 1024 {
			n = 16
		}
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return map[string]any{"hex": hex.EncodeToString(b)}, nil
	}
	t["hash.sha256"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		sum := sha256.Sum256([]byte(str(c.Args["data"], "")))
		return map[string]any{"hex": hex.EncodeToString(sum[:])}, nil
	}

	// 扩展调用（config/fs/route/schedule/log/events/auth）
	registerConfig(t, d)
	registerFS(t, d)
	registerRoute(t, d)
	registerSchedule(t, d)
	registerLog(t, d)
	registerEvents2(t, d)
	registerAuth(t, d)

	return t
}

// checkPerm 是所有调用的统一权限闸门（包级，供各注册文件复用）。
//
// 不在此处放行的调用一律拒绝；未知调用名直接 unknown_call——
// 这条规则让"实现了却忘记登记权限"在运行期立即暴露（见 scripts/check-syscall-contract.py）。
func checkPerm(d Deps, call Call) *CallError {
	need, ok := CallPermissions[call.Name]
	if !ok {
		return &CallError{Code: "unknown_call", Message: call.Name}
	}
	if need == "" {
		return nil
	}
	role := d.Policy.RoleOf(call.Caller.Subject)
	if !d.Policy.Allows(role, string(need)) {
		return &CallError{Code: "denied", Message: fmt.Sprintf("role %s lacks %s", role, need)}
	}
	return nil
}

// allow 保留为薄封装，让各注册文件读起来一致。
func allow(d Deps, c Call, args map[string]any) *CallError { return checkPerm(d, c) }

// ---- 参数转换辅助（syscall 参数来自 JSON，类型不确定） ----

func str(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func i64(v any, def int64) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return def
}

func strSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, x := range m {
		out[k] = fmt.Sprint(x)
	}
	return out
}

func parseSteps(v any) []task.Step {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]task.Step, 0, len(arr))
	for _, x := range arr {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, task.Step{
			Name:         str(m["name"], "step"),
			Argv:         strSlice(m["argv"]),
			Env:          strMap(m["env"]),
			Cwd:          str(m["cwd"], ""),
			Timeout:      int(i64(m["timeout_ms"], 0)),
			AllowFailure: m["allow_failure"] == true,
		})
	}
	return out
}

// SplitName 便于外部按 "域.动作" 拆分（如校验器使用）。
func SplitName(name string) (string, string) {
	i := strings.IndexByte(name, '.')
	if i < 0 {
		return name, ""
	}
	return name[:i], name[i+1:]
}
