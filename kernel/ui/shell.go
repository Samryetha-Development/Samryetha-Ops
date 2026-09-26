// Package ui 实现前端外壳的服务端部分。
//
// 外壳的职责边界（docs/architecture.md §9）：
//   - 只做：布局、路由、认证入口、**插槽渲染**、设计令牌下发
//   - 不做：任何业务。业务由服务通过 ui.declare 声明，外壳按插槽渲染。
//
// 为什么服务端生成而不是 SPA：WebSocket 实时 + 无构建步骤 + 单二进制自带 UI，
// 这三点对"部署工具"比 SPA 的灵活性更重要。复杂交互由外壳内联的一小段 JS 负责。
package ui

import (
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"sync"
)

// Slot 是插槽 id。
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

// AllSlots 是外壳支持的插槽全集。顺序决定页面上的渲染顺序。
var AllSlots = []Slot{
	SlotOverviewCards, SlotTabs, SlotActions, SlotSettingsSection,
	SlotLogSources, SlotCommands, SlotHotkeys, SlotToasts, SlotModals, SlotFooter,
}

// Component 是插件能声明的**受限组件**。
//
// 关键约束：插件不能注入任意 DOM，只能声明这些类型 + 数据。
// 外壳负责把类型渲染成继承设计令牌的 HTML——这是"通用"与"设计不崩"得以共存的原因。
type Component struct {
	// Kind 决定渲染方式
	Kind string `json:"kind"`
	// ID 供前端脚本引用（如按钮点击）
	ID string `json:"id,omitempty"`
	// Title/Label/Text 是通用文本字段
	Title string `json:"title,omitempty"`
	Label string `json:"label,omitempty"`
	Text  string `json:"text,omitempty"`
	// Value 用于 keyvalue/stat 类
	Value string `json:"value,omitempty"`
	// Items 用于 list/table/keyvalue/timeline/tree
	Items []Item `json:"items,omitempty"`
	// Columns 用于 table
	Columns []string `json:"columns,omitempty"`
	// Rows 用于 table
	Rows [][]string `json:"rows,omitempty"`
	// Tone 用于颜色语义：ok|warn|err|muted
	Tone string `json:"tone,omitempty"`
	// Action 用于 button：前端点击后调用的 syscall 或 API
	Action *Action `json:"action,omitempty"`
	// Confirm 是二次确认文案（从 Action 提上来，模板只读 Component 层）
	Confirm string `json:"confirm,omitempty"`
	// Fields 用于 form
	Fields []Field `json:"fields,omitempty"`
	// Bars 用于 chart（迷你柱状图）
	Bars []float64 `json:"bars,omitempty"`
	// Order 决定同插槽内顺序
	Order int `json:"order,omitempty"`
	// Source 标记组件来自哪个插件（用于"外部插件"标注）
	Source string `json:"source,omitempty"`
}

// Item 是列表/键值/时间线的一项。
type Item struct {
	Key   string `json:"key,omitempty"`
	Label string `json:"label,omitempty"`
	Value string `json:"value,omitempty"`
	Text  string `json:"text,omitempty"`
	At    int64  `json:"at,omitempty"`
	Tone  string `json:"tone,omitempty"`
}

// Action 描述一个可点击动作。
type Action struct {
	// Kind: api（调内核/服务 API）| link（跳转）
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Method string `json:"method,omitempty"`
	// Confirm 非空时前端需二次确认
	Confirm string `json:"confirm,omitempty"`
	// Body 是可选请求体
	Body string `json:"body,omitempty"`
}

// Field 描述表单字段。
type Field struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Type  string   `json:"type"` // text|int|bool|enum|secret|textarea
	Value string   `json:"value,omitempty"`
	Enum  []string `json:"enum,omitempty"`
	Help  string   `json:"help,omitempty"`
}

// NavItem 是导航项。
type NavItem struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
	Order int    `json:"order,omitempty"`
	Href  string `json:"href,omitempty"`
	// Source 标记来源插件
	Source string `json:"source,omitempty"`
}

// Declaration 是服务向外壳提交的 UI 声明（对应 syscall ui.declare）。
type Declaration struct {
	Source string                 `json:"source"` // 哪个服务声明的
	Slots  map[string][]Component `json:"slots"`  // 插槽 → 组件
	Nav    []NavItem              `json:"nav,omitempty"`
	// Sandbox 非空表示该服务要求用 iframe 承载（外部插件逃生舱）
	Sandbox string `json:"sandbox,omitempty"`
}

// Shell 收集所有服务的声明并渲染。
type Shell struct {
	mu    sync.RWMutex
	decls map[string]*Declaration
}

func NewShell() *Shell { return &Shell{decls: map[string]*Declaration{}} }

// Declare 登记一个服务的 UI 声明（幂等：同 source 覆盖）。
func (s *Shell) Declare(d *Declaration) {
	if d.Slots == nil {
		d.Slots = map[string][]Component{}
	}
	s.mu.Lock()
	s.decls[d.Source] = d
	s.mu.Unlock()
}

// Withdraw 撤销一个服务的声明（服务停止时调用，UI 立即反映）。
func (s *Shell) Withdraw(source string) {
	s.mu.Lock()
	delete(s.decls, source)
	s.mu.Unlock()
}

// Components 返回某插槽的全部组件（按 Order 稳定排序）。
func (s *Shell) Components(slot Slot) []Component {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Component
	for _, d := range s.decls {
		for _, c := range d.Slots[string(slot)] {
			c.Source = d.Source
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// Nav 返回全部导航项（按 Order 排序）。
func (s *Shell) Nav() []NavItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []NavItem
	for _, d := range s.decls {
		for _, n := range d.Nav {
			n.Source = d.Source
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// Sources 返回已声明的服务列表（供调试与"已加载服务"展示）。
func (s *Shell) Sources() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.decls))
	for k := range s.decls {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- 渲染 ---

// RenderData 是交给模板的完整数据。
type RenderData struct {
	Title      string
	Subtitle   string
	Nav        []NavItem
	Slots      map[string][]Component
	Sources    []string
	KernelMeta map[string]any
	// Rescue 非空表示处于救援模式（渲染救援界面）
	Rescue *RescueInfo
	Email  string
	CSRF   string
}

type RescueInfo struct {
	Reason string
	Phase  string
	Notes  []string
}

// Render 渲染整页。
func (s *Shell) Render(d RenderData) (string, error) {
	if d.Slots == nil {
		d.Slots = map[string][]Component{}
	}
	for _, slot := range AllSlots {
		if _, ok := d.Slots[string(slot)]; !ok {
			d.Slots[string(slot)] = s.Components(slot)
		}
	}
	funcs := template.FuncMap{
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		// 无 tone 时返回空串（普通样式）；只有显式语义才上色。
		// 早前实现把所有缺省值都涂成 muted，导致正常数值变灰。
		"toneClass": func(t string) string {
			switch t {
			case "ok", "warn", "err", "muted":
				return "tone-" + t
			}
			return ""
		},
		"nonempty": func(v any) bool {
			switch x := v.(type) {
			case string:
				return x != ""
			case []Component:
				return len(x) > 0
			case []NavItem:
				return len(x) > 0
			case []Item:
				return len(x) > 0
			case []Field:
				return len(x) > 0
			case []float64:
				return len(x) > 0
			case [][]string:
				return len(x) > 0
			}
			return false
		},
	}
	t, err := template.New("shell").Funcs(funcs).Parse(shellHTML)
	if err != nil {
		return "", fmt.Errorf("template parse: %w", err)
	}
	var buf stringsBuilder
	if err := t.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// stringsBuilder 避免 import strings 只为 builder（保持包自足）。
type stringsBuilder struct{ b []byte }

func (s *stringsBuilder) Write(p []byte) (int, error) { s.b = append(s.b, p...); return len(p), nil }
func (s *stringsBuilder) String() string              { return string(s.b) }
