// Package route 实现内核的动态路由挂载。
//
// 服务通过 route.mount 注册 HTTP 处理器与 WebSocket 端点；内核只负责分发，
// 不理解路径的业务含义。这让"加一个页面/接口"不需要改内核。
package route

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Handler 是服务提供的处理器（与标准 http.Handler 兼容）。
type Handler func(w http.ResponseWriter, r *http.Request)

// Mount 是一条已挂载的路由。
type Mount struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Source string `json:"source"`
	IsWS   bool   `json:"is_ws"`
	Public bool   `json:"public"` // 是否免认证（如 healthz）
}

// Table 是路由表。
type Table struct {
	mu     sync.RWMutex
	mounts []mountEntry
}

type mountEntry struct {
	m  Mount
	fn Handler
}

func New() *Table { return &Table{} }

// Mount 注册一条路由。冲突（同方法同路径）直接报错——
// 让两个服务抢同一路径是配置错误，应当显式失败而不是后者静默覆盖。
func (t *Table) Mount(m Mount, fn Handler) error {
	if m.Path == "" || !strings.HasPrefix(m.Path, "/") {
		return fmt.Errorf("path must start with /: %q", m.Path)
	}
	if m.Method == "" {
		m.Method = http.MethodGet
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.mounts {
		if e.m.Path == m.Path && e.m.Method == m.Method {
			return fmt.Errorf("route %s %s already mounted by %s", m.Method, m.Path, e.m.Source)
		}
	}
	t.mounts = append(t.mounts, mountEntry{m: m, fn: fn})
	return nil
}

// Unmount 撤销某服务挂载的全部路由（服务停止时）。
func (t *Table) Unmount(source string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.mounts[:0]
	n := 0
	for _, e := range t.mounts {
		if e.m.Source == source {
			n++
			continue
		}
		out = append(out, e)
	}
	t.mounts = out
	return n
}

// Lookup 按路径与方法查找处理器。返回 (nil,false) 表示没有匹配。
func (t *Table) Lookup(method, path string) (Handler, Mount, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// 精确匹配优先，其次前缀匹配（用于 /api/x/* 这类）
	var best *mountEntry
	for i := range t.mounts {
		e := &t.mounts[i]
		if e.m.Method != method && e.m.Method != "*" {
			continue
		}
		if e.m.Path == path || strings.HasSuffix(e.m.Path, "/*") &&
			strings.HasPrefix(path, strings.TrimSuffix(e.m.Path, "*")) {
			if best == nil || len(e.m.Path) > len(best.m.Path) {
				best = e
			}
		}
	}
	if best == nil {
		return nil, Mount{}, false
	}
	return best.fn, best.m, true
}

// Mounts 返回全部路由此表（稳定顺序，供 UI 与文档）。
func (t *Table) Mounts() []Mount {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Mount, 0, len(t.mounts))
	for _, e := range t.mounts {
		out = append(out, e.m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}
