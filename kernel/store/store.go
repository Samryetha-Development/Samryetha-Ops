// Package store 实现内核的键值存储：插件按命名空间隔离，互不可见。
//
// 内核只提供"命名空间 + 键 + 值"，不理解值的含义。持久化用简单的 JSON 段文件，
// 避免引入数据库依赖（内核应尽量少依赖）。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store 是命名空间隔离的 KV。
type Store struct {
	mu    sync.RWMutex
	dir   string
	data  map[string]map[string]string // ns -> key -> value
	dirty bool
}

// Open 从目录加载。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, data: map[string]map[string]string{}}
	b, err := os.ReadFile(filepath.Join(dir, "store.json"))
	if err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	return s, nil
}

// ns 校验：命名空间不得越权访问（防 "a/../b" 这类路径逃逸）。
func (s *Store) ns(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty namespace")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid namespace %q", name)
	}
	return name, nil
}

// Get 读一个键。
func (s *Store) Get(ns, key string) (string, bool, error) {
	n, err := s.ns(ns)
	if err != nil {
		return "", false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[n][key]
	return v, ok, nil
}

// Set 写一个键。
func (s *Store) Set(ns, key, val string) error {
	n, err := s.ns(ns)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.data[n] == nil {
		s.data[n] = map[string]string{}
	}
	s.data[n][key] = val
	s.dirty = true
	s.mu.Unlock()
	return s.flush()
}

// Del 删一个键。
func (s *Store) Del(ns, key string) error {
	n, err := s.ns(ns)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.data[n] != nil {
		delete(s.data[n], key)
	}
	s.dirty = true
	s.mu.Unlock()
	return s.flush()
}

// List 列出命名空间下所有键（可带前缀）。
func (s *Store) List(ns, prefix string) ([]string, error) {
	n, err := s.ns(ns)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data[n]))
	for k := range s.data[n] {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// flush 原子落盘（写临时文件 + rename），避免半写状态。
func (s *Store) flush() error {
	s.mu.RLock()
	b, err := json.Marshal(s.data)
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "store.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "store.json"))
}
