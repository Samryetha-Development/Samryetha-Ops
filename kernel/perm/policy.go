// Package perm 实现内核的权限判定：身份 → 角色 → 能力点。
//
// 内核只提供机制（谁能做什么）；身份来源是驱动（oidc/basic/token/none）。
// 内建角色 viewer/operator/admin；管理员可在配置里覆盖角色映射与能力点。
package perm

import (
	"fmt"
	"sort"
	"strings"
)

// Role 是内建角色。
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// builtinRoles 定义每个内建角色拥有的能力点。能力点形如 "域.动作"。
//
// 注意：内核**只定义机制类能力点**（log/proc/task/store/config/event/route）。
// 业务能力点（如 deploy.trigger）由插件在 manifest 里声明，经配置注入——
// 内核不得内置领域词汇，否则"内核不知道更新"这条不变量就破了。
var builtinRoles = map[Role][]string{
	RoleViewer: {
		"log.read", "event.read", "proc.read", "task.read", "config.read", "store.own",
	},
	RoleOperator: {
		"log.read", "event.read", "proc.read", "task.read", "config.read", "store.own",
		"event.emit", "proc.manage", "task.submit", "route.mount",
	},
	RoleAdmin: {"*"}, // 通配：全部能力
}

// Policy 是权限策略：角色定义 + 身份到角色的映射。
type Policy struct {
	// Extra 允许在配置里为角色追加能力点（不削减内建，避免误配导致自锁）。
	// 业务能力点（deploy.trigger 之类）应当走这里——内核不内置领域词汇。
	Extra map[Role][]string
	// Assign 是身份（OIDC sub 等不可变标识）到角色的映射。
	Assign map[string]Role
	// Default 是未映射身份的角色（默认 viewer=最小权限）。
	// 本地开发/内网可信场景可设为 admin，但生产必须保持 viewer。
	Default Role
}

// DefaultPolicy 返回一个空的策略（仅内建角色，无身份映射）。
func DefaultPolicy() *Policy {
	return &Policy{Extra: map[Role][]string{}, Assign: map[string]Role{}, Default: RoleViewer}
}

// RoleOf 返回身份所属角色。未映射者取 Default（默认 viewer，最小权限）。
//
// 立场（docs/architecture.md §7）：以不可变 sub 授权；邮箱可变、可被抢注，
// 因此不作为授权标识。若使用方坚持用邮箱，须自行确保 email_verified。
//
// 安全约束：角色**只能**来自策略，绝不采信请求头/调用方自述——
// 否则任何插件都能自称 admin。
func (p *Policy) RoleOf(subject string) Role {
	if r, ok := p.Assign[subject]; ok {
		return r
	}
	if p.Default != "" {
		return p.Default
	}
	return RoleViewer
}

// CapsOf 返回角色拥有的能力点集合（已展开 "*"）。
func (p *Policy) CapsOf(r Role) map[string]bool {
	out := map[string]bool{}
	for _, c := range builtinRoles[r] {
		out[c] = true
	}
	for _, c := range p.Extra[r] {
		out[c] = true
	}
	return out
}

// Allows 判定角色是否拥有某能力点。支持 "*" 通配与前缀通配（"deploy.*"）。
func (p *Policy) Allows(r Role, capability string) bool {
	caps := p.CapsOf(r)
	if caps["*"] || caps[capability] {
		return true
	}
	// 前缀通配
	for c := range caps {
		if strings.HasSuffix(c, ".*") && strings.HasPrefix(capability, strings.TrimSuffix(c, "*")) {
			return true
		}
	}
	return false
}

// Validate 检查配置里的角色映射是否合法（未知角色、空身份）。
func (p *Policy) Validate() error {
	known := map[Role]bool{RoleViewer: true, RoleOperator: true, RoleAdmin: true}
	for id, r := range p.Assign {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("perm: empty subject in assign")
		}
		if !known[r] {
			return fmt.Errorf("perm: subject %q maps to unknown role %q", id, r)
		}
	}
	for r := range p.Extra {
		if !known[r] {
			return fmt.Errorf("perm: extra capabilities for unknown role %q", r)
		}
	}
	return nil
}

// Roles 返回内建角色列表（稳定顺序，供 UI/文档使用）。
func Roles() []string {
	out := []string{string(RoleViewer), string(RoleOperator), string(RoleAdmin)}
	sort.Strings(out)
	return out
}
