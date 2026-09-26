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
