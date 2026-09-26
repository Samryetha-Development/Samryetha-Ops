package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"samryetha/kernel/actions"
	"samryetha/kernel/config"
	"samryetha/kernel/cron"
	"samryetha/kernel/fsops"
	"samryetha/kernel/route"
	"samryetha/services/deploycfg"
)

// wiring 汇总内核的扩展组件构造，保持 main 可读。
type wiring struct {
	Config  *config.Tree
	FS      *fsops.FS
	Routes  *route.Table
	Cron    *cron.Scheduler
	Actions *actions.Registry
	scopes  []fsops.Scope
}

// buildWiring 按配置构造扩展组件。
//
// 文件访问范围从 etc/scopes.json 读取（而不是硬编码）：
// 内核不知道"logs"是什么，它只知道"有个叫 logs 的范围，可读不可写"。
func buildWiring(root string) (*wiring, error) {
	w := &wiring{Routes: route.New(), Cron: cron.New(), Actions: actions.New()}

	tree, err := config.Open(root)
	if err != nil {
		return nil, err
	}
	w.Config = tree

	// 把 deploy.yaml 的内容并入配置树（含 managed_root 与服务配置段）。
	//
	// 顺序很重要：scope 构造要读 managed_root，因此必须在建 scope **之前**完成合并。
	// deploy.yaml 是"部署描述"，config.json 是"配置树"，两者本不该割裂——
	// 服务不该关心某个设置写在哪个文件里。
	if svcCfg, err := loadServiceConfig(root); err == nil {
		tree.MergeDefaults(svcCfg)
	}

	w.FS = fsops.New(8 << 20) // 单次读取上限 8MB

	// 被管理系统的根目录：内核自己的 root 是配置/状态目录（如 /opt/Samryetha/kernel），
	// 但它**管理的东西**在别处（如 /opt/Samryetha）。两者混为一谈会让 scope 指向内核自身，
	// 于是"写部署标记"就落到了没人看的目录里。
	managed := root
	if v, ok := tree.Get("managed_root"); ok {
		if s, ok := v.(string); ok && s != "" {
			managed = s
		}
	}
	if managed == root {
		// 未显式配置时：若父目录看起来是被管理的系统（含 backend/frontend），取父目录
		parent := filepath.Dir(root)
		if _, err := os.Stat(filepath.Join(parent, "backend")); err == nil {
			managed = parent
		}
	}

	// 默认范围（存在才登记，避免拒绝启动）
	defaults := []fsops.Scope{
		// logs 只读：日志是观测对象，服务不该改写它。
		{Name: "logs", Root: filepath.Join(managed, "logs"), Write: false},
		{Name: "etc", Root: filepath.Join(root, "etc"), Write: false},
		{Name: "var", Root: filepath.Join(root, "var"), Write: true},
		{Name: "status", Root: filepath.Join(managed, "status"), Write: true},
		// markers 可写：部署标记（.last-deployed / .last-deployed-dev）就放在 logs 下，
		// 状态页读的是这个路径，所以不能搬家——只能把"写标记"这一条授权单独拎出来。
		//
		// 为什么必须与 logs 分开：logs 是只读观测面，markers 是服务要写的状态。
		// 合成一个 scope 就等于"为了写一个标记而允许改写所有日志"，权限无从收紧。
		{Name: "markers", Root: filepath.Join(managed, "logs"), Write: true},
	}
	for _, s := range defaults {
		if _, err := os.Stat(s.Root); err == nil {
			_ = w.FS.Allow(s)
			w.scopes = append(w.scopes, s)
		}
	}
	// 配置补充的范围（同名字覆盖）
	if v, ok := tree.Get("fs.scopes"); ok {
		if arr, ok := v.([]any); ok {
			for _, x := range arr {
				m, ok := x.(map[string]any)
				if !ok {
					continue
				}
				name, _ := m["name"].(string)
				p, _ := m["path"].(string)
				write, _ := m["write"].(bool)
				if name != "" && p != "" {
					_ = w.FS.Allow(fsops.Scope{Name: name, Root: p, Write: write})
					w.scopes = append(w.scopes, fsops.Scope{Name: name, Root: p, Write: write})
				}
			}
		}
	}
	return w, nil
}

// serveRouted 是内核分发的入口：先查服务挂载的路由，未命中再走内核自带端点。
//
// 顺序很重要：服务可以覆盖同名的内核端点（例如自定义 /healthz），
// 但内核 API 前缀 /api/kernel/ 与 /_kernel/ 保留，不允许被覆盖。
func (w *wiring) serveRouted(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// 保留命名空间：内核自用，服务不得占用
		if len(r.URL.Path) >= 12 && r.URL.Path[:12] == "/api/kernel/" {
			fallback.ServeHTTP(rw, r)
			return
		}
		if len(r.URL.Path) >= 9 && r.URL.Path[:9] == "/_kernel/" {
			fallback.ServeHTTP(rw, r)
			return
		}
		if fn, m, ok := w.Routes.Lookup(r.Method, r.URL.Path); ok {
			// 公开路由免认证；其余交由处理器自行校验（处理器拿得到内核身份）
			_ = m
			fn(rw, r)
			return
		}
		fallback.ServeHTTP(rw, r)
	})
}

// writeJSON 通用响应（main 与各扩展共用）。
func writeJSONAny(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var _ = time.Now

// loadServiceConfig 从 etc/deploy.yaml 提取**服务配置段**（非 targets/schedule 的部分）。
//
// 这些段是给服务读的（如 statuspage.generator、deployer.health_retry），
// 与 targets（结构化部署描述）性质不同：前者是键值配置，后者是列表。
func loadServiceConfig(root string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(root, "etc", "deploy.yaml"))
	if err != nil {
		return nil, err
	}
	// 复用 deploycfg 的解析器，避免两套 YAML 解析（它们迟早会不一致）
	parsed, err := deploycfg.ParseRaw(string(b))
	if err != nil {
		return nil, err
	}
	// 只取服务配置段：排除部署描述本身的结构化字段
	skip := map[string]bool{
		"apiVersion": true, "project": true, "auth": true,
		"plugins": true, "targets": true, "schedule": true,
		"notify": true, "secrets": true,
		// 注意：managed_root 不跳过——内核需要它来定位被管理系统
	}
	out := map[string]any{}
	for k, v := range parsed {
		if skip[k] {
			continue
		}
		out[k] = v
	}
	return out, nil
}
