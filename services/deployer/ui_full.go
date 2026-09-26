package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"samryetha/sdk"
)

// DeclareFullUI 声明完整的运维面板。
//
// 与 DeclareUI 的区别：这里覆盖旧控制台的全部区块（进程/磁盘/提交/回滚版本/
// 备份/审计/配置提醒/日志），而不只是"部署"那一块。
//
// 设计不变式：deployer 只**声明**要显示什么，渲染由外壳负责。
// 因此每个区块都是"组件类型 + 数据"，没有任何 HTML。
func (s *Service) DeclareFullUI(ctx context.Context, plans []Plan, schedule map[string]string) error {
	panels := []map[string]any{}
	order := 10

	// ---- 进程状态 ----
	if procs, err := s.processStatus(ctx, plans); err == nil && len(procs) > 0 {
		items := []map[string]any{}
		for _, p := range procs {
			tone := "muted"
			if p.State == "online" || p.State == "active" {
				tone = "ok"
			} else if p.State != "" {
				tone = "err"
			}
			items = append(items, map[string]any{
				"label": p.Name, "value": fmt.Sprintf("%s · %s · 重启 %d", p.State, p.Mem, p.Restarts),
				"tone": tone,
			})
		}
		panels = append(panels, map[string]any{
			"kind": "keyvalue", "title": "进程", "order": order, "items": items,
		})
		order += 10
	}

	// ---- 磁盘与资源 ----
	if host, err := s.hostStats(ctx); err == nil && len(host) > 0 {
		items := []map[string]any{}
		for _, k := range []string{"disk", "mem", "load", "uptime"} {
			if v, ok := host[k]; ok {
				items = append(items, map[string]any{"label": k, "value": v})
			}
		}
		if len(items) > 0 {
			panels = append(panels, map[string]any{
				"kind": "keyvalue", "title": "磁盘与资源", "order": order, "items": items,
			})
			order += 10
		}
	}

	// ---- 可回滚版本 ----
	if vers := s.releaseDirs(ctx); len(vers) > 0 {
		items := []map[string]any{}
		for _, v := range vers {
			items = append(items, map[string]any{
				"label": v.sha, "value": v.when,
				"confirm": "确认回滚到 " + v.sha + "？",
				"action": map[string]any{
					"kind": "call", "target": "deployer.deploy",
					"body": fmt.Sprintf(`{"target":"%s"}`, v.target),
				},
			})
		}
		panels = append(panels, map[string]any{
			"kind": "list", "title": "可回滚版本", "order": order, "items": items,
		})
		order += 10
	}

	// ---- 最近提交 ----
	if commits, err := s.gitLog(ctx, plans); err == nil && len(commits) > 0 {
		items := []map[string]any{}
		for _, c := range commits {
			items = append(items, map[string]any{"label": c.sha, "value": c.subject})
		}
		panels = append(panels, map[string]any{
			"kind": "list", "title": "最近提交（main）", "order": order, "items": items,
		})
		order += 10
	}

	// ---- 数据库备份 ----
	if backups := s.backupList(ctx); len(backups) > 0 {
		rows := [][]string{}
		for _, b := range backups {
			rows = append(rows, []string{b.name, b.when})
		}
		panels = append(panels, map[string]any{
			"kind": "table", "title": "数据库备份", "order": order,
			"columns": []string{"备份", "时间"}, "rows": rows,
		})
		order += 10
	}

	// ---- 操作审计 ----
	if acts := s.auditList(ctx); len(acts) > 0 {
		rows := [][]string{}
		for _, a := range acts {
			rows = append(rows, []string{a.when, a.action, a.detail})
		}
		panels = append(panels, map[string]any{
			"kind": "table", "title": "操作审计", "order": order,
			"columns": []string{"时间", "动作", "详情"}, "rows": rows,
		})
		order += 10
	}

	// ---- 配置提醒 ----
	if notice := s.configNotice(ctx); notice != "" {
		panels = append(panels, map[string]any{
			"kind": "text", "title": "配置提醒", "order": order, "text": notice,
		})
		order += 10
	}

	// ---- 概览卡片与操作按钮（与 DeclareUI 相同，这里是完整版）----
	cards, actions, sections := s.buildTopLevel(ctx, plans, schedule)

	slots := map[string][]map[string]any{
		"overview.cards":    cards,
		"actions":           actions,
		"settings.sections": sections,
		"panels":            panels,
		"logs.sources": {
			{"kind": "text", "id": "deployment", "label": "deployment"},
			{"kind": "text", "id": "kernel", "label": "kernel"},
			{"kind": "text", "id": "statuspage", "label": "statuspage"},
		},
	}
	nav := []map[string]any{
		{"id": "overview", "label": "概览", "order": 10},
		{"id": "logs", "label": "日志", "order": 20},
	}
	if err := sdk.DeclareUI(s.K, slots, nav); err != nil {
		_ = s.K.Log("warn", "deployer ui.declare failed: "+err.Error(), nil)
		return err
	}
	_ = s.K.Log("info", fmt.Sprintf("deployer ui declared: %d cards, %d actions, %d panels",
		len(cards), len(actions), len(panels)), nil)
	return nil
}

// ---- 数据采集（全部经内核 syscall，不直接读文件/起进程）----

type procStat struct {
	Name, State, Mem string
	Restarts         int
}

func (s *Service) processStatus(ctx context.Context, plans []Plan) ([]procStat, error) {
	// 经内核跑 pm2 jlist / systemctl，避免服务自己起进程（保持"一切经 syscall"）
	var out []procStat
	seen := map[string]bool{}
	for _, p := range plans {
		var cmd string
		switch p.ProcessDriver {
		case "pm2":
			cmd = "pm2 jlist 2>/dev/null"
		case "systemd":
			for _, u := range p.ProcessNames {
				if !seen[u] {
					seen[u] = true
					out = append(out, procStat{Name: u, State: "unit"})
				}
			}
			continue
		default:
			continue
		}
		raw, err := s.runCapture(ctx, cmd)
		if err != nil {
			continue
		}
		for _, ps := range parsePM2Jlist(raw) {
			if !seen[ps.Name] {
				seen[ps.Name] = true
				out = append(out, ps)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type pm2App struct {
	Name   string `json:"name"`
	PM2Env struct {
		Status   string `json:"status"`
		Restarts int    `json:"restart_time"`
	} `json:"pm2_env"`
	Monit struct {
		Memory float64 `json:"memory"`
	} `json:"monit"`
}

func parsePM2Jlist(raw string) []procStat {
	var apps []pm2App
	if err := json.Unmarshal([]byte(raw), &apps); err != nil {
		return nil
	}
	var out []procStat
	for _, a := range apps {
		out = append(out, procStat{
			Name: a.Name, State: a.PM2Env.Status,
			Mem:      fmt.Sprintf("%.0fMB", a.Monit.Memory/1024/1024),
			Restarts: a.PM2Env.Restarts,
		})
	}
	return out
}

func (s *Service) hostStats(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if v, err := s.runCapture(ctx, "df -h / | tail -1 | awk '{print $5}'"); err == nil {
		out["disk"] = strings.TrimSpace(v) + " 已用"
	}
	if v, err := s.runCapture(ctx, "free -m | awk '/Mem:/{printf \"%.0f%%\", $3/$2*100}'"); err == nil {
		out["mem"] = strings.TrimSpace(v)
	}
	if v, err := s.runCapture(ctx, "uptime | sed 's/.*load average: //'"); err == nil {
		out["load"] = strings.TrimSpace(v)
	}
	if v, err := s.runCapture(ctx, "uptime -p"); err == nil {
		out["uptime"] = strings.TrimSpace(v)
	}
	return out, nil
}

type verEntry struct{ sha, when, target string }

func (s *Service) releaseDirs(ctx context.Context) []verEntry {
	// 回滚版本来自内核的 rollback 目录（deployer 保留的旧产物）
	raw, err := sdk.FsList(s.K, "markers:")
	if err != nil {
		return nil
	}
	_ = raw
	return nil // 内核尚未暴露 rollback 目录列表；保留接口，为空时不渲染该区块
}

type commit struct{ sha, subject string }

func (s *Service) gitLog(ctx context.Context, plans []Plan) ([]commit, error) {
	for _, p := range plans {
		if p.WorkDir == "" || p.Source != "git" {
			continue
		}
		v, err := s.runCapture(ctx, "git -C "+p.WorkDir+" log --oneline -8")
		if err != nil {
			continue
		}
		var out []commit
		for _, line := range strings.Split(strings.TrimSpace(v), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 2)
			c := commit{sha: parts[0]}
			if len(parts) > 1 {
				c.subject = parts[1]
			}
			out = append(out, c)
		}
		return out, nil
	}
	return nil, nil
}

type backup struct{ name, when string }

func (s *Service) backupList(ctx context.Context) []backup {
	v, err := s.runCapture(ctx, "ls -1dt /opt/Samryetha/logs/db-backup-* 2>/dev/null | head -10")
	if err != nil {
		return nil
	}
	var out []backup
	for _, line := range strings.Split(strings.TrimSpace(v), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "/")
		out = append(out, backup{name: parts[len(parts)-1]})
	}
	return out
}

type auditEntry struct{ when, action, detail string }

func (s *Service) auditList(ctx context.Context) []auditEntry {
	v, err := s.runCapture(ctx, "tail -8 /opt/Samryetha/logs/update.log 2>/dev/null")
	if err != nil || strings.TrimSpace(v) == "" {
		return nil
	}
	var out []auditEntry
	for _, line := range strings.Split(strings.TrimSpace(v), "\n") {
		if line == "" {
			continue
		}
		when, detail := "", line
		if len(line) > 19 {
			when, detail = line[:19], line[19:]
		}
		out = append(out, auditEntry{when: when, detail: strings.TrimSpace(detail)})
	}
	return out
}

func (s *Service) configNotice(ctx context.Context) string {
	if v, err := s.runCapture(ctx, "cat /opt/Samryetha/status/data/config-notice.json 2>/dev/null"); err == nil {
		t := strings.TrimSpace(v)
		if t != "" && t != "{}" {
			return t
		}
	}
	return ""
}

// runCapture 经内核跑命令并取回输出（服务不直接起进程）。
func (s *Service) runCapture(ctx context.Context, cmd string) (string, error) {
	taskID, err := s.K.Submit("capture", []sdk.Step{{
		Name: "capture", Argv: []string{"sh", "-lc", cmd}, Timeout: 30000,
	}}, 40000)
	if err != nil {
		return "", err
	}
	for i := 0; i < 200; i++ {
		st, err := s.K.Status(taskID)
		if err != nil {
			return "", err
		}
		switch st.State {
		case "succeeded":
			return strings.Join(st.Log, "\n"), nil
		case "failed", "canceled":
			return "", fmt.Errorf("%s", st.Err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
	}
	return "", fmt.Errorf("capture timeout")
}
