package ui

import "testing"

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
