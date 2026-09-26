// Package fsops 实现内核的受限文件访问。
//
// 为什么不让服务直接用 os.ReadFile：
//   - 路径逃逸（../）必须在**内核**统一拦住，否则每个服务各写一遍，迟早有一个写错
//   - 读写范围按 scope 白名单限定，插件只能碰配置允许的目录
//   - 大文件读取有上限，避免一个服务把内核内存吃光
//
// 内核不理解"日志""产物"是什么——它只知道"在允许的范围内读写文件"。
package fsops

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Scope 是允许访问的目录范围。
// 带 JSON tag：这些结构会直接出现在 API 响应里，缺 tag 会让前端读到 PascalCase 字段。
type Scope struct {
	Name  string `json:"name"`  // 作用域名（如 logs / releases）
	Root  string `json:"root"`  // 绝对路径
	Write bool   `json:"write"` // 是否允许写
}

// FS 是受限文件系统。
type FS struct {
	mu      sync.RWMutex
	scopes  map[string]Scope
	maxRead int64
}

func New(maxReadBytes int64) *FS {
	if maxReadBytes <= 0 {
		maxReadBytes = 4 << 20 // 4MB
	}
	return &FS{scopes: map[string]Scope{}, maxRead: maxReadBytes}
}

// Allow 注册一个访问范围（由内核在启动时按配置注入）。
func (f *FS) Allow(s Scope) error {
	abs, err := filepath.Abs(s.Root)
	if err != nil {
		return err
	}
	// 解析符号链接，避免用链接绕过范围检查
	real, err := filepath.EvalSymlinks(abs)
	if err == nil {
		abs = real
	}
	f.mu.Lock()
	f.scopes[s.Name] = Scope{Name: s.Name, Root: abs, Write: s.Write}
	f.mu.Unlock()
	return nil
}

// resolve 把 scope:relpath 解析为绝对路径，并验证仍在范围内。
//
// 判定用 filepath.Rel 而不是字符串前缀：
// 前缀比较会被 "/data-evil" 这类同前缀目录绕过（这是真实出现过的漏洞模式）。
func (f *FS) resolve(spec string, needWrite bool) (string, error) {
	i := strings.IndexByte(spec, ':')
	if i <= 0 {
		return "", fmt.Errorf("path must be scope:relpath (got %q)", spec)
	}
	name, rel := spec[:i], spec[i+1:]
	f.mu.RLock()
	sc, ok := f.scopes[name]
	f.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown scope %q", name)
	}
	if needWrite && !sc.Write {
		return "", fmt.Errorf("scope %q is read-only", name)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("relpath must be relative")
	}
	full := filepath.Join(sc.Root, rel)
	// 关键：必须在范围内
	r, err := filepath.Rel(sc.Root, full)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes scope %q", name)
	}
	return full, nil
}

// Read 读取文件（有大小上限）。
func (f *FS) Read(spec string) (string, error) {
	p, err := f.resolve(spec, false)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.Size() > f.maxRead {
		// 超限时只取尾部（日志场景最常用），而不是直接失败
		fh, err := os.Open(p)
		if err != nil {
			return "", err
		}
		defer fh.Close()
		if _, err := fh.Seek(-f.maxRead, 2); err != nil {
			return "", err
		}
		buf := make([]byte, f.maxRead)
		n, _ := fh.Read(buf)
		return string(buf[:n]), nil
	}
	b, err := os.ReadFile(p)
	return string(b), err
}

// Write 原子写（临时文件 + rename），避免读到半写内容。
func (f *FS) Write(spec, data string) error {
	p, err := f.resolve(spec, true)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// List 列目录。
func (f *FS) List(spec string) ([]string, error) {
	p, err := f.resolve(spec, false)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Scopes 返回已注册范围（供 UI 展示）。
func (f *FS) Scopes() []Scope {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Scope, 0, len(f.scopes))
	for _, s := range f.scopes {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
