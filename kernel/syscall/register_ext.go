package syscall

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"samryetha/kernel/config"
	"samryetha/kernel/fsops"
	"samryetha/kernel/perm"
	"samryetha/kernel/route"
	"samryetha/kernel/task"
	"samryetha/kernel/ui"
)

// registerConfig 挂上配置类调用。
func registerConfig(t Table, d Deps) {
	t["config.get"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.Config == nil {
			return nil, &CallError{Code: "unavailable", Message: "config not wired"}
		}
		path := str(c.Args["path"], "")
		// 密钥类键永不回传：即使配置里写了，也只在服务侧按引用解析
		if isSecretPath(path) {
			return nil, &CallError{Code: "denied", Message: "secret paths are not readable via config.get"}
		}
		v, ok := d.Config.Get(path)
		if !ok {
			return map[string]any{"found": false}, nil
		}
		return map[string]any{"found": true, "value": v}, nil
	}
	t["config.set"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.Config == nil {
			return nil, &CallError{Code: "unavailable", Message: "config not wired"}
		}
		path := str(c.Args["path"], "")
		if isSecretPath(path) {
			return nil, &CallError{Code: "denied", Message: "use the secrets driver for secret paths"}
		}
		if err := d.Config.Set(d.Root, path, c.Args["value"]); err != nil {
			return nil, &CallError{Code: "internal", Message: err.Error()}
		}
		return map[string]any{}, nil
	}
	t["config.list"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.Config == nil {
			return nil, &CallError{Code: "unavailable", Message: "config not wired"}
		}
		flat := d.Config.Flat()
		for k := range flat {
			if isSecretPath(k) {
				flat[k] = "<secret>"
			}
		}
		return map[string]any{"items": flat, "keys": d.Config.Keys()}, nil
	}
	t["config.watch"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		return map[string]any{"note": "watch is delivered as config.changed events"}, nil
	}
}

// registerFS 挂上受限文件访问。
func registerFS(t Table, d Deps) {
	t["fs.read"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.FS == nil {
			return nil, &CallError{Code: "unavailable", Message: "fs not wired"}
		}
		s, err := d.FS.Read(str(c.Args["path"], ""))
		if err != nil {
			return nil, &CallError{Code: "fs_error", Message: err.Error()}
		}
		return map[string]any{"data": s}, nil
	}
	t["fs.write"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.FS == nil {
			return nil, &CallError{Code: "unavailable", Message: "fs not wired"}
		}
		if err := d.FS.Write(str(c.Args["path"], ""), str(c.Args["data"], "")); err != nil {
			return nil, &CallError{Code: "fs_error", Message: err.Error()}
		}
		return map[string]any{}, nil
	}
	t["fs.list"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.FS == nil {
			return nil, &CallError{Code: "unavailable", Message: "fs not wired"}
		}
		items, err := d.FS.List(str(c.Args["path"], ""))
		if err != nil {
			return nil, &CallError{Code: "fs_error", Message: err.Error()}
		}
		return map[string]any{"items": items}, nil
	}
}

// registerRoute 挂上动态路由（服务提供处理器）。
func registerRoute(t Table, d Deps) {
	t["route.mount"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.Routes == nil {
			return nil, &CallError{Code: "unavailable", Message: "routes not wired"}
		}
		path := str(c.Args["path"], "")
		method := str(c.Args["method"], http.MethodGet)
		isWS := str(c.Args["kind"], "") == "ws"
		// 服务声明的是"我提供哪些端点"，实际处理器由该服务在**自己的进程内**提供；
		// 内建服务由内核直接调用，外部服务经反向代理（见 route.mount 的 mode 说明）。
		//
		// 这里登记元数据 + 一个占位处理器：真实转发在服务的 HTTP handler 里完成。
		err := d.Routes.Mount(route.Mount{
			Method: method, Path: path, Source: c.Caller.Plugin,
			IsWS: isWS, Public: c.Args["public"] == true,
		}, func(w http.ResponseWriter, r *http.Request) {
			// 内建服务在此处被直接调用（由服务在 mount 时提供闭包）；
			// 外部服务到达这里说明未接入转发——明确报错而不是静默 404。
			http.Error(w, "route declared by "+c.Caller.Plugin+" but no in-process handler bound", http.StatusNotImplemented)
		})
		if err != nil {
			return nil, &CallError{Code: "mount_failed", Message: err.Error()}
		}
		return map[string]any{}, nil
	}
	t["route.unmount"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.Routes == nil {
			return map[string]any{"removed": 0}, nil
		}
		return map[string]any{"removed": d.Routes.Unmount(c.Caller.Plugin)}, nil
	}
}

// registerSchedule 挂上任务类调用（once/cron）。
func registerSchedule(t Table, d Deps) {
	t["task.once"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		after := time.Duration(i64(c.Args["after_ms"], 0)) * time.Millisecond
		if after <= 0 {
			after = time.Second
		}
		name := str(c.Args["name"], "once")
		steps := parseSteps(c.Args["steps"])
		go func() {
			time.Sleep(after)
			d.Tasks.Submit(name, steps, int(i64(c.Args["timeout_ms"], 0)))
		}()
		return map[string]any{"scheduled_in_ms": after.Milliseconds()}, nil
	}
	t["task.cron"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.Cron == nil {
			return nil, &CallError{Code: "unavailable", Message: "cron not wired"}
		}
		spec := str(c.Args["spec"], "")
		if spec == "" {
			return nil, &CallError{Code: "invalid_args", Message: "spec required (5-field cron)"}
		}
		name := str(c.Args["name"], "cron")
		steps := parseSteps(c.Args["steps"])
		jobID, err := d.Cron.Add(name, spec, func() {
			d.Tasks.Submit(name, steps, 0)
		})
		if err != nil {
			return nil, &CallError{Code: "invalid_spec", Message: err.Error()}
		}
		return map[string]any{"job_id": jobID}, nil
	}
}

// registerLog 挂上日志查询（内核日志 + 服务日志统一视图）。
func registerLog(t Table, d Deps) {
	t["log.query"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		if d.LogQuery == nil {
			return map[string]any{"items": []any{}}, nil
		}
		limit := int(i64(c.Args["limit"], 200))
		q := str(c.Args["q"], "")
		src := str(c.Args["source"], "")
		return map[string]any{"items": d.LogQuery(src, q, limit)}, nil
	}
}

// registerEvents2 挂上订阅（以轮询游标形式暴露给外部服务；内建走 Go 通道）。
func registerEvents2(t Table, d Deps) {
	t["event.subscribe"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		if e := allow(d, c, c.Args); e != nil {
			return nil, e
		}
		// 外部服务无法持有内核通道：返回历史 + 游标，由服务按需轮询/长轮询。
		// 这是有意取舍：保持协议简单（HTTP/JSON），不引入双向流依赖。
		topic := str(c.Args["topic"], "")
		since := i64(c.Args["since"], 0)
		return map[string]any{
			"mode":  "cursor",
			"items": d.Bus.History(topic, since, 100),
		}, nil
	}
}

// registerAuth 挂上主体查询。
func registerAuth(t Table, d Deps) {
	t["auth.public"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		role := d.Policy.RoleOf(c.Caller.Subject)
		return map[string]any{
			"subject": c.Caller.Subject,
			"role":    role,
			"plugin":  c.Caller.Plugin,
			"caps":    capList(d, role),
		}, nil
	}
	t["auth.check"] = func(ctx context.Context, c Call) (map[string]any, *CallError) {
		capability := str(c.Args["capability"], "")
		role := d.Policy.RoleOf(c.Caller.Subject)
		return map[string]any{"allowed": d.Policy.Allows(role, capability), "role": role}, nil
	}
}

func capList(d Deps, role perm.Role) []string {
	caps := d.Policy.CapsOf(role)
	out := make([]string, 0, len(caps))
	for c := range caps {
		out = append(out, c)
	}
	return out
}

// isSecretPath 判定配置路径是否属于密钥（永不回传明文）。
func isSecretPath(p string) bool {
	lp := strings.ToLower(p)
	for _, k := range []string{"secret", "password", "passwd", "token", "key", "credential"} {
		// "key" 单独出现太宽（monkey/sortkey），要求是路径段才判定
		if k == "key" {
			for _, seg := range strings.Split(lp, ".") {
				if seg == "key" || seg == "keys" || strings.HasSuffix(seg, "_key") || strings.HasSuffix(seg, "_keys") {
					return true
				}
			}
			continue
		}
		if strings.Contains(lp, k) {
			return true
		}
	}
	return false
}

// 占位：避免 import 循环时的类型噪声
var _ = config.Tree{}
var _ = fsops.FS{}
var _ = task.Step{}
var _ = ui.Component{}
var _ = fmt.Sprint
