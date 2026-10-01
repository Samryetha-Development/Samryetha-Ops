package main

import (
	"testing"

	"samryetha/services/deploycfg"
)

// 配置数值有两条来源：deploy.yaml（自带解析器 → int）与 JSON 配置（→ float64）。
// 只认 float64 会让 deploy.yaml 里写下的数值**静默失效**——界面没有任何异常，
// 行为却永远停在默认值。这个缺陷是端到端烟雾测试抓到的。
func TestAsNumberAcceptsBothConfigSources(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{1, 1, true},        // deploy.yaml 的整数
		{int64(2), 2, true}, // 另一种整数形态
		{1.5, 1.5, true},    // JSON 配置的数值
		{"1", 0, false},     // 字符串不当数字：宁可留默认值，也不猜
		{nil, 0, false},     //
	}
	for _, c := range cases {
		got, ok := asNumber(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("asNumber(%#v) = (%v, %v)，期望 (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// 内核自己写的日志没有 plugin 字段，按来源筛选时必须归到 "kernel"，
// 否则控制台的"日志 → kernel"永远空白，看起来像内核从不写日志。
func TestSourceOfDefaultsToKernel(t *testing.T) {
	if got := sourceOf(kvLine{Msg: "x"}); got != "kernel" {
		t.Fatalf("无 meta 的日志应归为 kernel，实际 %q", got)
	}
	if got := sourceOf(kvLine{Meta: map[string]any{"plugin": "svc"}}); got != "svc" {
		t.Fatalf("带 plugin 的日志应按 plugin 归类，实际 %q", got)
	}
	if got := sourceOf(kvLine{Meta: map[string]any{"plugin": ""}}); got != "kernel" {
		t.Fatalf("plugin 为空串应归为 kernel，实际 %q", got)
	}
}

// 控制台的设置表单存的是 schedule.<id> / enabled.<id>；这两个键必须真的被读到，
// 否则表单就是"能填能选、什么都不发生"的摆设（本项目反复踩的病）。
func TestResolveSchedulePrefersConfigOverrides(t *testing.T) {
	def := deploycfg.Schedule{Target: "main", Cron: "*/5 * * * *", Enabled: true}

	// 没有任何覆盖：用描述文件的默认值
	cron, on := resolveSchedule(def, func(string) (any, bool) { return nil, false })
	if cron != "*/5 * * * *" || !on {
		t.Fatalf("无覆盖时应沿用默认值，实际 cron=%q on=%v", cron, on)
	}

	// 覆盖 cron：改调度必须生效
	cron, on = resolveSchedule(def, func(p string) (any, bool) {
		if p == "schedule.main" {
			return "*/2 * * * *", true
		}
		return nil, false
	})
	if cron != "*/2 * * * *" || !on {
		t.Fatalf("cron 覆盖未生效，实际 cron=%q on=%v", cron, on)
	}

	// 覆盖开关（控制台表单存的是字符串）：关掉就不调度
	_, on = resolveSchedule(def, func(p string) (any, bool) {
		if p == "enabled.main" {
			return "false", true
		}
		return nil, false
	})
	if on {
		t.Fatal("enabled=false 的覆盖未生效：仍在调度")
	}

	// 关闭开关 + 改 cron 同时存在：以开关为准
	cron, on = resolveSchedule(def, func(p string) (any, bool) {
		if p == "enabled.main" {
			return "false", true
		}
		if p == "schedule.main" {
			return "*/1 * * * *", true
		}
		return nil, false
	})
	if on || cron != "*/1 * * * *" {
		t.Fatalf("开关应压过 cron，实际 cron=%q on=%v", cron, on)
	}

	// 描述文件里本来就是关的：不调度
	if _, on := resolveSchedule(deploycfg.Schedule{Target: "dev", Cron: "*/5 * * * *"}, func(string) (any, bool) { return nil, false }); on {
		t.Fatal("schedule.enabled=false 时不应调度")
	}
}

func TestTruthyAcceptsStringAndBool(t *testing.T) {
	for _, in := range []any{true, "true", "TRUE", "1", "on", "yes", " true "} {
		if !truthy(in) {
			t.Fatalf("truthy(%#v) 应为 true", in)
		}
	}
	for _, in := range []any{false, "false", "0", "off", "", nil, "maybe"} {
		if truthy(in) {
			t.Fatalf("truthy(%#v) 应为 false", in)
		}
	}
}
