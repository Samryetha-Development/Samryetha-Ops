package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"samryetha/kernel/config"
	"samryetha/kernel/cron"
	"samryetha/kernel/fsops"
	"samryetha/kernel/route"
	"samryetha/services/deploycfg"
)

// wiring 汇总内核的扩展组件构造，保持 main 可读。
type wiring struct {
	Config *config.Tree
	FS     *fsops.FS
	Routes *route.Table
	Cron   *cron.Scheduler
	scopes []fsops.Scope
}

// buildWiring 按配置构造扩展组件。
//
// 文件访问范围从 etc/scopes.json 读取（而不是硬编码）：
// 内核不知道"logs"是什么，它只知道"有个叫 logs 的范围，可读不可写"。
func buildWiring(root string) (*wiring, error) {
	w := &wiring{Routes: route.New(), Cron: cron.New()}

	tree, err := config.Open(root)
	if err != nil {
		return nil, err
	}
	// 把 deploy.yaml 里**服务级**的配置段并入配置树。
	//
	// 为什么需要这一步：deploy.yaml 是"部署描述"（含 targets/schedule 这类结构化数据），
	// 而 config.get 读的是配置树（etc/config.json）。两者若各自独立，
	// 服务就会遇到"我明明写在 deploy.yaml 里了，为什么 config.get 读不到"。
	// 这里统一：deploy.yaml 中的服务配置段（statuspage/deployer/notify 等）
	// 自动并入配置树，作为**默认值**（config.json 与 var/config.json 仍可覆盖）。
	if svcCfg, err := loadServiceConfig(root); err == nil {
		tree.MergeDefaults(svcCfg)
	}
	w.Config = tree

	w.FS = fsops.New(8 << 20) // 单次读取上限 8MB
	// 默认范围：根目录下常见位置（存在才登记，避免拒绝启动）
	defaults := []fsops.Scope{
		{Name: "logs", Root: filepath.Join(root, "logs"), Write: false},
		{Name: "etc", Root: filepath.Join(root, "etc"), Write: false},
		{Name: "var", Root: filepath.Join(root, "var"), Write: true},
		{Name: "status", Root: filepath.Join(root, "status"), Write: true},
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
