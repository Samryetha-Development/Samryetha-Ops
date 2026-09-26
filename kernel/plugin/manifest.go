// Package plugin 定义插件 manifest、加载计划与兼容性校验。
//
// 内核在加载期校验：apiVersion 兼容、requires 可满足、permissions 在允许集合内。
// 不满足则拒绝加载并把原因写进内核日志——而不是半死不活地跑起来。
package plugin

import (
	"fmt"
	"sort"

	"samryetha/kernel/syscall"
)

// Kind 插件种类。
type Kind string

const (
	KindKernel  Kind = "kernel"  // 内核模块（in-proc，须最严格评审）
	KindService Kind = "service" // 用户态服务（默认）
	KindDriver  Kind = "driver"  // 驱动：适配外部系统
)

// Isolation 隔离级别（docs/architecture.md §5.2）。
type Isolation string

const (
	IsolationInproc  Isolation = "inproc"
	IsolationProcess Isolation = "process"
	IsolationSocket  Isolation = "socket"
)

// Slot 前端插槽（外壳预定义，插件只能往这些位置声明）。
type Slot string

const (
	SlotOverviewCards   Slot = "overview.cards"
	SlotTabs            Slot = "tabs"
	SlotActions         Slot = "actions"
	SlotSettingsSection Slot = "settings.sections"
	SlotLogSources      Slot = "logs.sources"
	SlotCommands        Slot = "commands"
	SlotHotkeys         Slot = "hotkeys"
	SlotToasts          Slot = "toasts"
	SlotModals          Slot = "modals"
	SlotFooter          Slot = "footer"
)

// KnownSlots 是外壳支持的插槽全集。声明未知插槽 = 加载失败（防插件与外壳版本错配）。
var KnownSlots = map[Slot]bool{
	SlotOverviewCards: true, SlotTabs: true, SlotActions: true,
	SlotSettingsSection: true, SlotLogSources: true, SlotCommands: true,
	SlotHotkeys: true, SlotToasts: true, SlotModals: true, SlotFooter: true,
}

// NavItem 导航项。
type NavItem struct {
	ID    string `yaml:"id" json:"id"`
	Label string `yaml:"label" json:"label"`
	Icon  string `yaml:"icon,omitempty" json:"icon,omitempty"`
	Order int    `yaml:"order,omitempty" json:"order,omitempty"`
}

// ConfigField 插件自述的配置项，内核据 type 渲染与校验，避免每个插件自带一套 UI。
type ConfigField struct {
	Key     string   `yaml:"key" json:"key"`
	Type    string   `yaml:"type" json:"type"` // string|int|bool|enum|secret|duration|path|list
	Default any      `yaml:"default,omitempty" json:"default,omitempty"`
	Enum    []string `yaml:"enum,omitempty" json:"enum,omitempty"`
	Help    string   `yaml:"help,omitempty" json:"help,omitempty"`
}

// Manifest 是 plugin.yaml 的结构。
type Manifest struct {
	ID           string        `yaml:"id" json:"id"`
	Version      string        `yaml:"version" json:"version"`
	APIVersion   string        `yaml:"apiVersion" json:"apiVersion"` // 依赖的 syscall 版本
	Kind         Kind          `yaml:"kind" json:"kind"`
	Isolation    Isolation     `yaml:"isolation" json:"isolation"`
	Capabilities []string      `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Requires     []string      `yaml:"requires,omitempty" json:"requires,omitempty"`
	Permissions  []string      `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Slots        []Slot        `yaml:"slots,omitempty" json:"slots,omitempty"`
	Nav          []NavItem     `yaml:"nav,omitempty" json:"nav,omitempty"`
	Config       []ConfigField `yaml:"config,omitempty" json:"config,omitempty"`
	Entry        string        `yaml:"entry,omitempty" json:"entry,omitempty"` // 外部插件的可执行入口
}

// Validate 校验 manifest 自身是否合法（不含依赖关系）。
func (m *Manifest) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("manifest: id is required")
	}
	if m.Version == "" {
		return fmt.Errorf("manifest %s: version is required", m.ID)
	}
	if !compatible(m.APIVersion) {
		return fmt.Errorf("manifest %s: apiVersion %q incompatible with kernel %q",
			m.ID, m.APIVersion, syscall.Version)
	}
	switch m.Kind {
	case KindKernel, KindService, KindDriver:
	default:
		return fmt.Errorf("manifest %s: unknown kind %q", m.ID, m.Kind)
	}
	switch m.Isolation {
	case IsolationInproc, IsolationProcess, IsolationSocket:
	case "":
		// 默认 process（见 §5.2：只有经评审的代码允许 inproc）
		m.Isolation = IsolationProcess
	default:
		return fmt.Errorf("manifest %s: unknown isolation %q", m.ID, m.Isolation)
	}
	// inproc 是内核崩溃预算内的特权，必须显式声明 kind=kernel
	if m.Isolation == IsolationInproc && m.Kind != KindKernel {
		return fmt.Errorf("manifest %s: isolation=inproc reserved for kind=kernel", m.ID)
	}
	for _, s := range m.Slots {
		if !KnownSlots[s] {
			return fmt.Errorf("manifest %s: unknown ui slot %q", m.ID, s)
		}
	}
	for _, p := range m.Permissions {
		if _, ok := syscall.CallPermissions[p]; !ok && !knownPermission(p) {
			return fmt.Errorf("manifest %s: unknown permission %q", m.ID, p)
		}
	}
	if m.Isolation != IsolationInproc && m.Entry == "" {
		return fmt.Errorf("manifest %s: external plugins require entry", m.ID)
	}
	return nil
}

// compatible 判定插件声明的 apiVersion 是否被当前内核支持。
// 策略（§6）：同大版本兼容；不同大版本拒绝。
func compatible(declared string) bool {
	if declared == "" {
		return false
	}
	return major(declared) == major(syscall.Version)
}

func major(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] == '/' {
			return v[i:]
		}
	}
	return v
}

// knownPermission 允许插件声明"能力点"式的自定义权限（域.动作），
// 与 syscall 权限点区分：前者是业务能力，后者是内核调用。
func knownPermission(p string) bool {
	dot := -1
	for i := 0; i < len(p); i++ {
		if p[i] == '.' {
			dot = i
			break
		}
	}
	return dot > 0 && dot < len(p)-1
}

// Plan 是加载计划。
type Plan struct {
	Order  []*Manifest
	Reason string // 若为空表示可加载
}

// BuildPlan 计算加载顺序（按依赖拓扑排序），并返回不可满足的原因。
//
// 规则：requires 里出现的名字，必须由某个已启用插件的 capabilities 提供。
// 缺失即拒绝整批加载——避免"起了一半"的中间态。
func BuildPlan(manifests []*Manifest) Plan {
	byCap := map[string]string{} // capability -> plugin id
	for _, m := range manifests {
		for _, c := range m.Capabilities {
			byCap[c] = m.ID
		}
	}
	// 校验依赖
	for _, m := range manifests {
		for _, r := range m.Requires {
			if _, ok := byCap[r]; !ok {
				return Plan{Reason: fmt.Sprintf("plugin %s requires %q but no enabled plugin provides it", m.ID, r)}
			}
		}
	}
	// 拓扑排序（依赖优先）
	idx := map[string]*Manifest{}
	for _, m := range manifests {
		idx[m.ID] = m
	}
	visited := map[string]int{} // 0 未访问 1 访问中 2 完成
	var order []*Manifest
	var visit func(id string) error
	visit = func(id string) error {
		switch visited[id] {
		case 1:
			return fmt.Errorf("dependency cycle at %s", id)
		case 2:
			return nil
		}
		visited[id] = 1
		m := idx[id]
		for _, r := range m.Requires {
			if dep := byCap[r]; dep != "" && dep != id {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		visited[id] = 2
		order = append(order, m)
		return nil
	}
	ids := make([]string, 0, len(manifests))
	for id := range idx {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 稳定输出
	for _, id := range ids {
		if err := visit(id); err != nil {
			return Plan{Reason: err.Error()}
		}
	}
	return Plan{Order: order}
}
