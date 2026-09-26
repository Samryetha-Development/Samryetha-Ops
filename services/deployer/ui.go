package deployer

import (
	"context"
	"fmt"
	"time"

	"samryetha/sdk"
)

// RegisterActions 把部署/回滚注册为可由 UI 触发的动作。
//
// 关键：内核不知道"部署"是什么，它只知道"deployer 注册了一个叫 deployer.deploy 的动作"。
// 这是"内核保持领域中立"与"界面需要业务按钮"之间的正解。
func (s *Service) RegisterActions(plans []Plan) error {
	byID := map[string]Plan{}
	for _, p := range plans {
		byID[p.ID] = p
	}

	if err := s.K.(interface {
		RegisterAction(name string, h func(ctx context.Context, args map[string]any) (map[string]any, error)) error
	}).RegisterAction("deployer.deploy", func(ctx context.Context, args map[string]any) (map[string]any, error) {
		id, _ := args["target"].(string)
		plan, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown target %q", id)
		}
		out := s.Deploy(ctx, plan)
		return map[string]any{"state": out.State, "target": out.Target,
			"from": out.From, "to": out.To, "error": out.Err}, nil
	}); err != nil {
		return err
	}

	return s.K.(interface {
		RegisterAction(name string, h func(ctx context.Context, args map[string]any) (map[string]any, error)) error
	}).RegisterAction("deployer.rollback", func(ctx context.Context, args map[string]any) (map[string]any, error) {
		id, _ := args["target"].(string)
		plan, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown target %q", id)
		}
		return s.RollbackToPrevious(ctx, plan)
	})
}

// DeclareUI 向内核外壳声明部署面板。
//
// 这是"控制台"的正确做法：服务声明"我要显示什么、有哪些按钮"，
// 外壳负责渲染与交互。服务不产出 HTML——因此设计风格由外壳统一保证，
// 换一个项目无需改写界面代码。
func (s *Service) DeclareUI(ctx context.Context, plans []Plan, schedule map[string]string) error {
	cards, actions, sections := s.buildTopLevel(ctx, plans, schedule)
	slots := map[string][]map[string]any{
		"overview.cards":    cards,
		"actions":           actions,
		"settings.sections": sections,
		"logs.sources": {
			{"kind": "text", "id": "deployment", "label": "deployment"},
			{"kind": "text", "id": "kernel", "label": "kernel"},
			{"kind": "text", "id": "statuspage", "label": "statuspage"},
		},
	}
	nav := []map[string]any{
		{"id": "deploy", "label": "部署", "order": 10},
		{"id": "logs", "label": "日志", "order": 20},
	}
	// 诊断：声明失败必须可见（此前失败被上层 warn 吞掉，表现为"页面什么都没有"）
	if err := sdk.DeclareUI(s.K, slots, nav); err != nil {
		_ = s.K.Log("warn", "deployer ui.declare failed: "+err.Error(), nil)
		return err
	}
	_ = s.K.Log("info", fmt.Sprintf("deployer ui declared: %d cards, %d actions, %d sections",
		len(cards), len(actions), len(sections)), nil)
	return nil
}

// targetState 读取某目标最近一次部署的结果（来自内核事件流）。

// buildTopLevel 构造概览卡片、操作按钮与设置区。
// 抽出来让完整面板与精简面板共用同一份构造逻辑，避免两处各写一遍、慢慢长歪。
func (s *Service) buildTopLevel(ctx context.Context, plans []Plan, schedule map[string]string) ([]map[string]any, []map[string]any, []map[string]any) {
	cards := make([]map[string]any, 0, len(plans))
	actions := make([]map[string]any, 0)
	sections := make([]map[string]any, 0)
	for i, p := range plans {
		// 读取该目标的已部署标记与实际 HEAD，呈现"是否落后"
		deployed := ""
		if p.Marker != "" {
			if v, err := sdk.FsRead(s.K, p.Marker); err == nil && v != "" {
				deployed = short(trimNewline(v))
			}
		}
		st := s.targetState(ctx, p)
		tone := "muted"
		label := "unknown"
		switch st.state {
		case "succeeded":
			tone, label = "ok", "已部署"
		case "failed":
			tone, label = "err", "失败"
		case "rolled_back":
			tone, label = "warn", "已回滚"
		case "running":
			tone, label = "warn", "进行中"
		}
		val := label
		if deployed != "" {
			val = fmt.Sprintf("%s · %s", label, deployed)
		}
		cards = append(cards, map[string]any{
			"kind": "stat", "title": p.ID, "value": val, "tone": tone, "order": 10 + i,
		})

		// 每个目标一组动作按钮
		actions = append(actions,
			map[string]any{
				"kind": "button", "label": "部署 " + p.ID, "tone": "ok", "order": 100 + i*10,
				"confirm": fmt.Sprintf("确认部署 %s（分支 %s）？", p.ID, p.Branch),
				"action": map[string]any{
					"kind": "call", "target": "deployer.deploy",
					"body": fmt.Sprintf(`{"target":%q}`, p.ID),
				},
			},
			map[string]any{
				"kind": "button", "label": "回滚 " + p.ID, "tone": "err", "order": 101 + i*10,
				"confirm": fmt.Sprintf("确认回滚 %s 到上一版本？", p.ID),
				"action": map[string]any{
					"kind": "call", "target": "deployer.rollback",
					"body": fmt.Sprintf(`{"target":%q}`, p.ID),
				},
			},
		)

		// 设置区：调度与开关
		if cron, ok := schedule[p.ID]; ok {
			sections = append(sections, map[string]any{
				"kind": "form", "title": p.ID + " 调度", "order": 10 + i,
				"fields": []map[string]any{
					{"key": "schedule." + p.ID, "label": "cron", "type": "text", "value": cron,
						"help": "5 段 cron，例如 */5 * * * *；留空表示不自动部署"},
					{"key": "enabled." + p.ID, "label": "启用自动部署", "type": "bool",
						"value": fmt.Sprint(p.enabled())},
				},
			})
		}
	}

	return cards, actions, sections
}

func (s *Service) targetState(ctx context.Context, p Plan) struct{ state string } {
	var out struct{ state string }
	// 通过内核事件历史查询最近一条 deployment.* 且 target 匹配的事件
	ev, err := sdk.CallGeneric(s.K, "event.history", map[string]any{"topic": "deployment", "limit": 100})
	if err != nil {
		return out
	}
	items, _ := ev["items"].([]any)
	// 从后往前找该目标最近的事件
	for i := len(items) - 1; i >= 0; i-- {
		m, _ := items[i].(map[string]any)
		if m == nil {
			continue
		}
		if m["source"] == nil {
			continue
		}
		pl, _ := m["payload"].(map[string]any)
		if pl == nil || pl["target"] != p.ID {
			continue
		}
		st, _ := pl["state"].(string)
		out.state = st
		return out
	}
	return out
}

func (p Plan) enabled() bool { return true }

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

var _ = time.Now
