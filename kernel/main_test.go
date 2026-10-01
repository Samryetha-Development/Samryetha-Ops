package main

import "testing"

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
