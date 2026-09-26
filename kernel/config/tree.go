// Package config 实现内核的配置树：分层来源 + 只读暴露 + 变更通知。
//
// 内核只负责"从哪读、怎么合并、什么时候变了"，不理解任何具体配置项的含义。
// 来源分层（后者覆盖前者）：
//
//	defaults（程序内建） → etc/config.json（部署配置） → var/config.json（运行时覆盖）
//
// 密钥不在此层：密钥由 secrets 驱动按引用解析（见 services/drivers），
// 且永不写入日志/store。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Tree 是合并后的配置树。
type Tree struct {
	mu      sync.RWMutex
	data    map[string]any
	watcher []func(path string)
}

// Open 加载配置树。缺失的文件按空处理（不报错——配置可以只有默认值）。
func Open(root string) (*Tree, error) {
	t := &Tree{data: map[string]any{}}
	for _, f := range []string{
		filepath.Join(root, "etc", "config.json"),
		filepath.Join(root, "var", "config.json"),
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("config %s: %w", f, err)
		}
		t.data = merge(t.data, m)
	}
	return t, nil
}

// Get 按点号路径取值，支持 map 逐层下钻。找不到返回 (nil,false)。
func (t *Tree) Get(path string) (any, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if path == "" {
		return t.data, true
	}
	cur := any(t.data)
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Set 写入运行时覆盖层并落盘（服务改配置的唯一合法路径）。
func (t *Tree) Set(root, path string, val any) error {
	t.mu.Lock()
	setPath(t.data, strings.Split(path, "."), val)
	t.mu.Unlock()
	if err := t.flush(root); err != nil {
		return err
	}
	t.notify(path)
	return nil
}

// Watch 订阅变更（返回取消函数）。
func (t *Tree) Watch(fn func(path string)) func() {
	t.mu.Lock()
	t.watcher = append(t.watcher, fn)
	idx := len(t.watcher) - 1
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if idx < len(t.watcher) {
			t.watcher[idx] = func(string) {}
		}
	}
}

// Flat 返回扁平化后的键值（供 UI 展示）。
func (t *Tree) Flat() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, vv)
			}
		default:
			out[prefix] = fmt.Sprint(v)
		}
	}
	walk("", t.data)
	return out
}

// Keys 返回顶层键（稳定顺序）。
func (t *Tree) Keys() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.data))
	for k := range t.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (t *Tree) notify(path string) {
	t.mu.RLock()
	ws := append([]func(string){}, t.watcher...)
	t.mu.RUnlock()
	for _, w := range ws {
		w(path)
	}
}

func (t *Tree) flush(root string) error {
	t.mu.RLock()
	b, _ := json.MarshalIndent(t.data, "", "  ")
	t.mu.RUnlock()
	dir := filepath.Join(root, "var")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "config.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "config.json"))
}

func merge(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

func setPath(m map[string]any, keys []string, val any) {
	for i := 0; i < len(keys)-1; i++ {
		nxt, ok := m[keys[i]].(map[string]any)
		if !ok {
			nxt = map[string]any{}
			m[keys[i]] = nxt
		}
		m = nxt
	}
	m[keys[len(keys)-1]] = val
}
