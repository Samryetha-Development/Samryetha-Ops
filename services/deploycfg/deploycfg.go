// Package deploycfg 把 etc/deploy.yaml 解析成内核服务可用的结构。
//
// 内核不解析它（内核不知道 yaml 意味着什么）；由用户态服务读取，
// 因此换项目 = 换这份文件，内核与驱动零改动。
//
// 为保持内核与服务的零外部依赖，这里实现 YAML 的**受限子集**解析：
// 仅支持 deploy.yaml 实际用到的结构（嵌套 map / 列表 / 标量与内联数组）。
// 这不是通用 YAML——文档里明确了限制，遇到不支持的结构会明确报错而非静默忽略。
package deploycfg

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"samryetha/services/deployer"
	drivers "samryetha/services/drivers"
)

// File 是解析后的部署描述。
type File struct {
	APIVersion string
	Project    map[string]any
	Auth       map[string]any
	Plugins    map[string][]string
	Targets    []deployer.Plan
	Schedule   []Schedule
	Notify     []Notify
	Raw        map[string]any
}

type Schedule struct {
	Target  string
	Cron    string
	Enabled bool
}

type Notify struct {
	Driver string
	Events []string
	URL    string
}

// ParseRaw 暴露底层解析器：让其它组件（如内核的配置合并）复用同一套 YAML 子集解析，
// 避免出现两套解析器各自演化、迟早不一致。
func ParseRaw(src string) (map[string]any, error) { return parseYAML(src) }

// Load 读取并解析。
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root, err := parseYAML(string(b))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	f := &File{Raw: root}
	f.APIVersion, _ = root["apiVersion"].(string)
	f.Project, _ = root["project"].(map[string]any)
	f.Auth, _ = root["auth"].(map[string]any)

	if pl, ok := root["plugins"].(map[string]any); ok {
		f.Plugins = map[string][]string{}
		for k, v := range pl {
			f.Plugins[k] = toStrSlice(v)
		}
	}
	for _, t := range toAnySlice(root["targets"]) {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		f.Targets = append(f.Targets, toPlan(m))
	}
	for _, s := range toAnySlice(root["schedule"]) {
		if m, ok := s.(map[string]any); ok {
			f.Schedule = append(f.Schedule, Schedule{
				Target: str(m["target"]), Cron: str(m["cron"]),
				Enabled: m["enabled"] != false,
			})
		}
	}
	for _, n := range toAnySlice(root["notify"]) {
		if m, ok := n.(map[string]any); ok {
			f.Notify = append(f.Notify, Notify{
				Driver: str(m["driver"]), Events: toStrSlice(m["events"]), URL: str(m["url"]),
			})
		}
	}
	return f, nil
}

// toPlan 把配置里的一条 target 转成可执行计划。
func toPlan(m map[string]any) deployer.Plan {
	p := deployer.Plan{
		ID:      str(m["id"]),
		Branch:  str(m["branch"]),
		WorkDir: str(m["workdir"]),
		Build:   toStrSlice(m["build"]),
		Restart: toStrSlice(m["restart"]),
		Keep:    toInt(m["keep"], 3),
		Marker:  str(m["marker"]),
	}
	// source：字符串或映射（取 driver 字段）
	switch v := m["source"].(type) {
	case string:
		p.Source = v
	case map[string]any:
		p.Source = str(v["driver"])
	default:
		p.Source = "git"
	}
	// health 列表
	for _, h := range toAnySlice(m["health"]) {
		if hm, ok := h.(map[string]any); ok {
			p.Health = append(p.Health, drivers.HealthSpec{
				Type: str(hm["type"]), URL: str(hm["url"]), Addr: str(hm["addr"]),
				Command: str(hm["command"]), Expect: str(hm["expect"]), Timeout: toInt(hm["timeout"], 0),
			})
		}
	}
	// processes
	if pm, ok := m["processes"].(map[string]any); ok {
		p.ProcessDriver = str(pm["driver"])
		p.ProcessNames = toStrSlice(pm["names"])
		if p.ProcessNames == nil {
			p.ProcessNames = toStrSlice(pm["units"])
		}
	}
	// migrations
	if mm, ok := m["migrations"].(map[string]any); ok {
		p.MigrationDriver = str(mm["driver"])
		p.MigrationDetect = str(mm["detect"])
		p.MigrationCmd = str(mm["command"])
		p.BackupBefore = mm["backup_before"] == true
	}
	// hooks
	if hm, ok := m["hooks"].(map[string]any); ok {
		p.BeforeDeploy = toStrSlice(hm["before_deploy"])
		p.AfterDeploy = toStrSlice(hm["after_deploy"])
	}
	return p
}

// ---- YAML 子集解析 ----
//
// 支持：
//   key: value                 标量
//   key:                       嵌套块
//     child: value
//   - item                     列表项（标量或映射）
//   key: [a, b, c]             内联数组
//   "quoted": "...", 注释 #
// 不支持：锚点/引用、多行折叠、复杂嵌套列表。遇到即明确报错。

type parser struct {
	lines []string
	pos   int
}

func parseYAML(src string) (map[string]any, error) {
	var lines []string
	for _, l := range strings.Split(src, "\n") {
		// 去注释（保留引号内的 #）
		l = stripComment(l)
		if strings.TrimSpace(l) == "" {
			continue
		}
		lines = append(lines, l)
	}
	p := &parser{lines: lines}
	v, err := p.parseBlock(0)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("top level must be a mapping")
	}
	return m, nil
}

func stripComment(l string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(l); i++ {
		switch l[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return l[:i]
			}
		}
	}
	return l
}

func (p *parser) indent(l string) int {
	n := 0
	for n < len(l) && l[n] == ' ' {
		n++
	}
	return n
}

// parseBlock 解析同一缩进层级下的映射或列表。
func (p *parser) parseBlock(minIndent int) (any, error) {
	if p.pos >= len(p.lines) {
		return map[string]any{}, nil
	}
	first := p.lines[p.pos]
	ind := p.indent(first)
	if ind < minIndent {
		return map[string]any{}, nil
	}
	trimmed := strings.TrimSpace(first)

	// 列表
	if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
		var out []any
		for p.pos < len(p.lines) {
			l := p.lines[p.pos]
			if p.indent(l) < ind {
				break
			}
			t := strings.TrimSpace(l)
			if !strings.HasPrefix(t, "-") {
				break
			}
			itemText := strings.TrimSpace(strings.TrimPrefix(t, "-"))
			if itemText == "" {
				// 列表项是嵌套块
				p.pos++
				sub, err := p.parseBlock(ind + 1)
				if err != nil {
					return nil, err
				}
				out = append(out, sub)
				continue
			}
			if isMappingLine(itemText) {
				// 列表项是内联映射：把 "- key: v" 当作缩进后的块
				// 重写当前行为其子内容，再解析后续同缩进行
				childIndent := ind + 2
				p.lines[p.pos] = strings.Repeat(" ", childIndent) + itemText
				sub, err := p.parseBlock(childIndent)
				if err != nil {
					return nil, err
				}
				out = append(out, sub)
				continue
			}
			out = append(out, parseScalar(itemText))
			p.pos++
		}
		return out, nil
	}

	// 映射
	out := map[string]any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if p.indent(l) < ind {
			break
		}
		if p.indent(l) > ind {
			return nil, fmt.Errorf("unexpected indentation: %q", strings.TrimSpace(l))
		}
		t := strings.TrimSpace(l)
		key, rest, ok := splitKey(t)
		if !ok {
			return nil, fmt.Errorf("expected key: value, got %q", t)
		}
		if rest == "" {
			// 嵌套块
			p.pos++
			sub, err := p.parseBlock(ind + 1)
			if err != nil {
				return nil, err
			}
			out[key] = sub
			continue
		}
		out[key] = parseScalar(rest)
		p.pos++
	}
	return out, nil
}

func isMappingLine(s string) bool {
	_, _, ok := splitKey(s)
	return ok
}

// splitKey 拆 "key: rest"，正确处理引号内的冒号。
func splitKey(s string) (string, string, bool) {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ':':
			if !inSingle && !inDouble {
				k := strings.TrimSpace(s[:i])
				if k == "" {
					return "", "", false
				}
				return unquote(k), strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

func parseScalar(s string) any {
	s = strings.TrimSpace(s)
	// 内联数组
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []any{}
		}
		var out []any
		for _, part := range splitTop(inner, ',') {
			out = append(out, parseScalar(part))
		}
		return out
	}
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if s == "null" || s == "~" {
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return unquote(s)
}

// splitTop 按分隔符切分，忽略引号内的分隔符。
func splitTop(s string, sep byte) []string {
	var out []string
	inSingle, inDouble := false, false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		default:
			if s[i] == sep && !inSingle && !inDouble {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func toInt(v any, def int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

func toAnySlice(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func toStrSlice(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, str(e))
		}
		return out
	case string:
		return []string{x}
	}
	return nil
}
