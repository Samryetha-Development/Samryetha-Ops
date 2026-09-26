package main

import (
	"context"
	"encoding/json"
	"fmt"

	"samryetha/kernel/cron"
	"samryetha/kernel/syscall"
	"samryetha/kernel/task"
	"samryetha/sdk"
)

// cronScheduler 是内核 cron 的最小接口（避免直接依赖具体类型）。
type cronScheduler interface {
	SetHook(name string, fn func())
}

var _ cronScheduler = (*cron.Scheduler)(nil)

// 内建服务的主体身份。
//
// 设计考量：内建服务由内核自己装载（deployer/statuspage），它们需要 task.submit、
// proc.manage 这类能力，但**不该**完全绕过权限（那会让审计与边界失效）。
// 因此给它们一个显式的、可审计的主体标识，并在策略里映射为 operator——
// 权限仍然经过同一条判定路径，只是在策略里显式授权。
//
// 这也意味着：外部插件**不能**自称这个主体（身份由认证驱动注入，不来自请求）。
const BuiltinSubject = "builtin:kernel"

// syscallAdapter 把内核的 syscall 表暴露为 sdk.Kernel。
//
// 这是"内建服务"路径：服务通过同一张 syscall 表调用内核，
// 与外部服务（经 HTTP/JSON-RPC）在语义上完全一致，只是零序列化。
//
// 因此同一份服务代码既能内建（in-process）也能外置——这是通用性的关键。
type syscallAdapter struct {
	table   syscall.Table
	plugin  string
	subject string
	// cron 让内建服务把函数绑定到定时任务（见 sdk.BindCron）。
	// 只注册不绑定会造成"到点触发却什么都不做"的静默空转。
	cron cronScheduler
}

func newSyscallAdapter(table syscall.Table, plugin string) *syscallAdapter {
	return &syscallAdapter{table: table, plugin: plugin, subject: BuiltinSubject}
}

// call 是所有方法共用的入口：构造 Call、执行、解包。
func (a *syscallAdapter) call(name string, args map[string]any) (map[string]any, error) {
	h, ok := a.table[name]
	if !ok {
		return nil, fmt.Errorf("unknown syscall %s", name)
	}
	data, cerr := h(context.Background(), syscall.Call{
		Name: name, Args: args,
		Caller: syscall.CallerInfo{Plugin: a.plugin, Subject: a.subject},
	})
	if cerr != nil {
		return nil, cerr
	}
	return data, nil
}

// Call 实现 sdk.Extended 泛化调用（新 syscall 无需改接口即可使用）。
func (a *syscallAdapter) Call(name string, args map[string]any) (map[string]any, error) {
	return a.call(name, args)
}

func (a *syscallAdapter) Log(level, msg string, fields map[string]any) error {
	_, err := a.call("log.write", map[string]any{"level": level, "msg": msg, "fields": fields})
	return err
}

func (a *syscallAdapter) Emit(topic string, payload map[string]any) (string, error) {
	d, err := a.call("event.emit", map[string]any{"topic": topic, "payload": payload})
	if err != nil {
		return "", err
	}
	id, _ := d["id"].(string)
	return id, nil
}

func (a *syscallAdapter) StoreGet(ns, key string) (string, bool, error) {
	d, err := a.call("store.get", map[string]any{"key": key})
	if err != nil {
		return "", false, err
	}
	v, _ := d["value"].(string)
	found, _ := d["found"].(bool)
	return v, found, nil
}

func (a *syscallAdapter) StoreSet(ns, key, val string) error {
	_, err := a.call("store.set", map[string]any{"key": key, "value": val})
	return err
}

func (a *syscallAdapter) StoreDel(ns, key string) error {
	_, err := a.call("store.del", map[string]any{"key": key})
	return err
}

func (a *syscallAdapter) StoreList(ns, prefix string) ([]string, error) {
	d, err := a.call("store.list", map[string]any{"prefix": prefix})
	if err != nil {
		return nil, err
	}
	return toStrList(d["keys"]), nil
}

func (a *syscallAdapter) ConfigGet(path string) (any, error) {
	d, err := a.call("config.get", map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	return d["value"], nil
}

func (a *syscallAdapter) Spawn(argv []string, env map[string]string, cwd string) (int, error) {
	d, err := a.call("proc.spawn", map[string]any{"argv": argv, "env": env, "cwd": cwd})
	if err != nil {
		return 0, err
	}
	return toInt(d["pid"]), nil
}

func (a *syscallAdapter) SpawnCapture(argv []string, env map[string]string, cwd string) (int, error) {
	d, err := a.call("proc.spawn", map[string]any{"argv": argv, "env": env, "cwd": cwd, "capture": true})
	if err != nil {
		return 0, err
	}
	return toInt(d["pid"]), nil
}

func (a *syscallAdapter) Output(pid int) (string, error) {
	d, err := a.call("proc.output", map[string]any{"pid": pid})
	if err != nil {
		return "", err
	}
	s, _ := d["output"].(string)
	return s, nil
}

func (a *syscallAdapter) Signal(pid int, sig string) error {
	_, err := a.call("proc.signal", map[string]any{"pid": pid, "signal": sig})
	return err
}

func (a *syscallAdapter) Wait(pid int, timeoutMs int) (int, error) {
	d, err := a.call("proc.wait", map[string]any{"pid": pid, "timeout_ms": timeoutMs})
	if err != nil {
		return -1, err
	}
	return toInt(d["code"]), nil
}

func (a *syscallAdapter) List() ([]sdk.ProcInfo, error) {
	d, err := a.call("proc.list", nil)
	if err != nil {
		return nil, err
	}
	var out []sdk.ProcInfo
	if arr, ok := d["procs"].([]any); ok {
		for _, x := range arr {
			if m, ok := x.(map[string]any); ok {
				out = append(out, sdk.ProcInfo{
					PID: toInt(m["pid"]), Argv: strOf(m["argv"]),
					Running: m["running"] == true, Uptime: int64(toInt(m["uptime_ms"])),
				})
			}
		}
	}
	return out, nil
}

func (a *syscallAdapter) Submit(name string, steps []sdk.Step, timeoutMs int) (string, error) {
	// sdk.Step 与 kernel/task.Step 字段一致，直接序列化传递
	b, _ := json.Marshal(steps)
	var arr []any
	_ = json.Unmarshal(b, &arr)
	d, err := a.call("task.submit", map[string]any{"name": name, "steps": arr, "timeout_ms": timeoutMs})
	if err != nil {
		return "", err
	}
	id, _ := d["task_id"].(string)
	return id, nil
}

func (a *syscallAdapter) Status(taskID string) (sdk.TaskState, error) {
	d, err := a.call("task.status", map[string]any{"task_id": taskID})
	if err != nil {
		return sdk.TaskState{}, err
	}
	// task.status 返回的是 task.State 结构体（不是 map）——内建路径下它按原样传递，
	// 因此这里要做类型分支；只断言 map 会让所有内建任务调用都报 "no task state"。
	switch v := d["task"].(type) {
	case task.State:
		return sdk.TaskState{
			ID: v.ID, Name: v.Name, State: v.State, Step: v.Current,
			ExitCode: v.ExitCode, Log: v.Log, Err: v.Err,
		}, nil
	case map[string]any:
		return sdk.TaskState{
			ID: strOf(v["id"]), Name: strOf(v["name"]), State: strOf(v["state"]),
			Step: strOf(v["current"]), ExitCode: toInt(v["exit_code"]),
			Log: toStrList(v["log"]), Err: strOf(v["err"]),
		}, nil
	}
	return sdk.TaskState{}, fmt.Errorf("no task state")
}

func (a *syscallAdapter) Cancel(taskID string) error {
	_, err := a.call("task.cancel", map[string]any{"task_id": taskID})
	return err
}

// ---- 小工具 ----

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func toStrList(v any) []string {
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

var _ = task.Step{}

// SetCronHook 把动作绑定到已注册的定时任务（实现 sdk.cronHookSetter）。
//
// 这是"注册"与"动作"解耦的关键：cron 由内核负责到点触发，
// 但"到点做什么"属于服务。缺了这一步，定时任务会按时触发却什么都不做。
func (a *syscallAdapter) SetCronHook(name string, fn func()) {
	if a.cron != nil {
		a.cron.SetHook(name, fn)
	}
}
