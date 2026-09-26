package sdk

import (
	"context"
	"encoding/json"
)

// 本文件补齐服务侧需要、但 Kernel 主接口未覆盖的 syscall 便捷方法。
//
// 设计意图：Kernel 接口只放**最常用**的调用；其余通过 Call 泛化调用，
// 避免接口随 syscall 增长而膨胀（每新增一个调用就改接口会破坏插件兼容）。
type Extended interface {
	// Call 泛化调用任意 syscall（逃生舱：新调用无需改接口即可使用）
	Call(name string, args map[string]any) (map[string]any, error)
}

// 以下便捷方法通过 Call 实现，供内建服务直接使用。

func call(k Kernel, name string, args map[string]any) (map[string]any, error) {
	e, ok := k.(Extended)
	if !ok {
		return nil, errNotExtended(name)
	}
	return e.Call(name, args)
}

type extendErr struct{ name string }

func (e extendErr) Error() string {
	return "kernel adapter does not implement Extended.Call, cannot invoke " + e.name
}

func errNotExtended(name string) error { return extendErr{name} }

// Cron 注册定时任务（内核不理解任务内容，只负责到点触发）。
//
// 注意：这只注册了"到点触发"，**动作仍需绑定**（见 SetCronHook）。
// 若只注册不绑定，任务会按时触发却什么都不做——
// 这种"静默空转"很难发现，因此推荐直接用 BindCron 一步完成。
func Cron(k Kernel, spec, name string, steps []Step) (string, error) {
	d, err := call(k, "task.cron", map[string]any{"spec": spec, "name": name, "steps": steps})
	if err != nil {
		return "", err
	}
	id, _ := d["job_id"].(string)
	return id, nil
}

// cronHookSetter 由内核适配器实现，用于把动作绑定到已注册的定时任务。
type cronHookSetter interface {
	SetCronHook(name string, fn func())
}

// BindCron 注册定时任务**并绑定动作**（推荐入口）。
//
// 内建服务用这个；外部服务（跨进程）无法在核内存活函数，
// 应改用"内核触发 → 通过 proc.spawn 调自己的 CLI"的方式（见文档）。
func BindCron(k Kernel, spec, name string, fn func()) (string, error) {
	id, err := Cron(k, spec, name, []Step{{Name: name, Argv: []string{"true"}}})
	if err != nil {
		return "", err
	}
	if setter, ok := k.(cronHookSetter); ok {
		setter.SetCronHook(name, fn)
	}
	return id, nil
}

// MountRoute 声明一条由本服务提供的路由。
func MountRoute(k Kernel, method, path string, public bool) error {
	_, err := call(k, "route.mount", map[string]any{
		"method": method, "path": path, "public": public,
	})
	return err
}

// UnmountRoutes 撤销本服务的全部路由。
func UnmountRoutes(k Kernel) (int, error) {
	d, err := call(k, "route.unmount", nil)
	if err != nil {
		return 0, err
	}
	n, _ := d["removed"].(float64)
	return int(n), nil
}

// DeclareUI 向外壳声明插槽内容。
func DeclareUI(k Kernel, slots map[string][]map[string]any, nav []map[string]any) error {
	_, err := call(k, "ui.declare", map[string]any{"slots": slots, "nav": nav})
	return err
}

// WithdrawUI 撤销 UI 声明（服务停止时，界面立即反映）。
func WithdrawUI(k Kernel) error {
	_, err := call(k, "ui.withdraw", nil)
	return err
}

// FsRead / FsWrite / FsList 走受限文件访问（路径形如 "logs:update.log"）。
func FsRead(k Kernel, path string) (string, error) {
	d, err := call(k, "fs.read", map[string]any{"path": path})
	if err != nil {
		return "", err
	}
	s, _ := d["data"].(string)
	return s, nil
}

func FsWrite(k Kernel, path, data string) error {
	_, err := call(k, "fs.write", map[string]any{"path": path, "data": data})
	return err
}

func FsList(k Kernel, path string) ([]string, error) {
	d, err := call(k, "fs.list", map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	var out []string
	if arr, ok := d["items"].([]any); ok {
		for _, x := range arr {
			out = append(out, stringify(x))
		}
	}
	return out, nil
}

// ConfigGet / ConfigSet 走配置树（密钥路径不可读）。
func ConfigGet(k Kernel, path string) (any, bool, error) {
	d, err := call(k, "config.get", map[string]any{"path": path})
	if err != nil {
		return nil, false, err
	}
	found, _ := d["found"].(bool)
	return d["value"], found, nil
}

func ConfigSet(k Kernel, path string, val any) error {
	_, err := call(k, "config.set", map[string]any{"path": path, "value": val})
	return err
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// CallGeneric 是对 call 的公开包装：服务需要调用未封装成便捷函数的 syscall 时使用。
//
// 存在意义：新增 syscall 不必立刻进 sdk 接口（那会破坏插件兼容），
// 但服务仍需要一个受权限约束的统一入口。
func CallGeneric(k Kernel, name string, args map[string]any) (map[string]any, error) {
	return call(k, name, args)
}

// RegisterAction 把一个内建动作注册到内核（可由 UI 按钮触发）。
//
// 动作名必须带服务前缀（如 "deployer.deploy"），内核据此归属与授权。
// 内核不知道动作做什么——它只负责按名字派发与权限检查。
func RegisterAction(k Kernel, name string, h func(ctx context.Context, args map[string]any) (map[string]any, error)) error {
	e, ok := k.(interface {
		RegisterAction(string, func(context.Context, map[string]any) (map[string]any, error)) error
	})
	if !ok {
		return errNotExtended(name)
	}
	return e.RegisterAction(name, h)
}
