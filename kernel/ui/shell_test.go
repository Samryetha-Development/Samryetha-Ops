package ui

import (
	"strings"
	"testing"
)

// 模板（theme.go）会渲染这些插槽；它们必须都在 AllSlots 里。
//
// 回归背景：deployer 一直声明 "panels"，模板也会渲染它，但它不在 AllSlots——
// 而 Render 只回填 AllSlots 中的插槽，于是面板声明全部落空、页面上什么都没有，
// 且不报任何错。这类"声明成功但永不渲染"只有靠断言把插槽清单钉死才能防住。
func TestAllSlotsCoversRenderedSlots(t *testing.T) {
	rendered := []Slot{
		SlotOverviewCards, SlotActions, SlotPanels, SlotSettingsSection, SlotLogSources,
	}
	for _, s := range rendered {
		if !IsKnownSlot(string(s)) {
			t.Errorf("插槽 %q 会被模板渲染，却不在 AllSlots 中：声明将静默失效", s)
		}
	}
	if IsKnownSlot("definitely-not-a-slot") {
		t.Fatal("未知插槽不应被认定为已知")
	}
}

// 设置卡片必须同时提供"保存"和"恢复默认"两条路径。
//
// 回归背景：这些输入框曾经**完全没有保存路径**（没有按钮、没有收集代码），
// 而"保存"一旦存在却没有"恢复默认"，用户改一次就再也回不到跟随描述文件的状态
// （覆盖值优先级更高，且他看不到它）。所以两个按钮都要在模板里，且带 data-field
// 的字段必须真的渲染出来，否则按钮点了也没数据可提交。
func TestSettingsCardRendersSaveAndReset(t *testing.T) {
	shell := NewShell()
	html, err := shell.Render(RenderData{
		Slots: map[string][]Component{
			string(SlotSettingsSection): {{
				Kind:  "form",
				Title: "示例调度",
				Fields: []Field{
					{Key: "schedule.x", Label: "cron", Type: "text", Value: "*/5 * * * *"},
					{Key: "enabled.x", Label: "开关", Type: "bool", Value: "true"},
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="settings-save"`,
		`id="settings-reset"`,
		`data-field="schedule.x"`,
		`value="*/5 * * * *"`,
		`data-field="enabled.x"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("设置卡片缺少 %s（按钮点了没数据、或没有撤销入口）", want)
		}
	}
}
