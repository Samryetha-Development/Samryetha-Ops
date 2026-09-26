// Package actions 实现"服务动作"注册表。
//
// 内核不知道"部署""回滚"是什么——但界面需要按钮能触发它们。
// 解法不是把领域知识塞进内核，而是让**服务自己注册动作**：
//
//	服务：Register("deployer.deploy", ...) 并在注册时提供处理器
//	UI：  invoke("deployer.deploy") → 内核按名字找到处理器并转发
//
// 内核全程只知道"有个叫 X 的动作、它属于哪个服务"，不知道 X 做什么。
// 动作名强制带服务前缀（deployer.*），避免两个服务抢同一个名字。
package actions

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Handler 是服务提供的动作实现。
type Handler func(ctx context.Context, args map[string]any) (map[string]any, error)

// Registry 是动作注册表。
type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
}

type entry struct {
	owner   string
	handler Handler
}

func New() *Registry { return &Registry{entries: map[string]entry{}} }

// Register 注册一个动作。handler 可为 nil（外部服务经由自身进程执行，
// 内核只做名字到属主的登记）。
//
// 重复注册同名动作会覆盖——同一服务重启时要能重新登记，不能因此失败。
func (r *Registry) Register(name, owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.entries[name]
	if ok && prev.owner != owner {
		// 不同服务抢同名动作：拒绝后来者（前缀约定应已避免，这里是兜底）
		return
	}
	r.entries[name] = entry{owner: owner, handler: prev.handler}
}

// RegisterHandler 注册动作并绑定处理器（内建服务用这个）。
func (r *Registry) RegisterHandler(name, owner string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[name] = entry{owner: owner, handler: h}
}

// Unregister 撤销某服务的全部动作（服务停止时）。
func (r *Registry) Unregister(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, e := range r.entries {
		if e.owner == owner {
			delete(r.entries, k)
			n++
		}
	}
	return n
}

// Owner 返回动作的属主服务。
func (r *Registry) Owner(name string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	return e.owner, ok
}

// Invoke 执行动作。
func (r *Registry) Invoke(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	r.mu.RLock()
	e, ok := r.entries[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown action %s", name)
	}
	if e.handler == nil {
		return nil, fmt.Errorf("action %s has no in-process handler (owner %s runs externally)", name, e.owner)
	}
	return e.handler(ctx, args)
}

// List 返回全部动作（稳定顺序，供 UI/文档）。
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for k := range r.entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
