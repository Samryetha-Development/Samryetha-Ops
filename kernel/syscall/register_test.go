package syscall

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"samryetha/kernel/events"
	"samryetha/kernel/fsops"
	"samryetha/kernel/perm"
)

// 事件历史必须在两条调用路径上给出**同一种形态**。
//
// 入参的两种形态（HTTP/JSON 的 map vs 内建的原始 Go 类型）早已有转换辅助；
// 返回值此前没有：内建服务是 Go 直调，拿到的是 events.Envelope 结构体，
// 而服务侧只会按 map 解析（`items[i].(map[string]any)`），断言静默失败。
// 后果不是报错，而是"事件明明发了、history 也确实返回了，却什么都读不到"。
func TestEnvelopeMapsReturnsMapsOnDirectCallPath(t *testing.T) {
	envs := []events.Envelope{{
		ID: "e1", Topic: "svc.finished", TS: 1700000000000,
		Source:  "service:x",
		Payload: map[string]any{"target": "t1", "state": "succeeded"},
	}}

	items := envelopeMaps(envs)
	if len(items) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(items))
	}
	m, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("直调路径必须给出 map，实际 %T（调用方会因此读不到任何字段）", items[0])
	}
	if m["topic"] != "svc.finished" || m["source"] != "service:x" {
		t.Fatalf("字段丢失：%#v", m)
	}
	if ts, ok := m["ts"].(int64); !ok || ts != 1700000000000 {
		t.Fatalf("ts 形态不对：%#v", m["ts"])
	}
	pl, ok := m["payload"].(map[string]any)
	if !ok || pl["state"] != "succeeded" {
		t.Fatalf("payload 丢失：%#v", m["payload"])
	}
}

func TestEnvelopeMapsHandlesEmpty(t *testing.T) {
	if got := envelopeMaps(nil); len(got) != 0 {
		t.Fatalf("空历史应给出空列表，实际 %#v", got)
	}
}

// 直接打真实的 event.history 处理器（而不是只测转换函数）：
// 这样把 handler 改回"原样返回结构体"时，本测试必须失败。
func TestEventHistoryHandlerGivesMaps(t *testing.T) {
	bus := events.New(16, nil)
	bus.Emit("svc.finished", "service:x", nil, map[string]any{"state": "ok"})

	policy := perm.DefaultPolicy()
	policy.Assign["test:subject"] = perm.RoleOperator
	table := Register(Deps{Bus: bus, Policy: policy})

	data, cerr := table["event.history"](context.Background(), Call{
		Name:   "event.history",
		Args:   map[string]any{"topic": "svc", "limit": 10},
		Caller: CallerInfo{Plugin: "t", Subject: "test:subject"},
	})
	if cerr != nil {
		t.Fatalf("event.history 失败：%v", cerr)
	}
	items, ok := data["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("应返回 1 条，实际 %#v", data["items"])
	}
	m, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("直调路径必须给出 map，实际 %T", items[0])
	}
	if m["topic"] != "svc.finished" {
		t.Fatalf("topic 字段不对：%#v", m)
	}
}

// 组件内的 items / fields / bars 必须容忍**内建服务的 Go 类型**。
//
// 回归背景：`buildComponent` 曾用 `m["items"].([]any)` 断言。外部经 HTTP/JSON
// 传进来的确实是 []any，而内建服务传的是 []map[string]any——断言失败不报错，
// 结果是**面板有标题、内容全空**：进程、磁盘与资源、最近提交、设置里的表单
// 全都被这样吞掉。控制台上看起来"功能没做"，实际是数据被丢在解析这一步。
func TestBuildComponentAcceptsGoTypedItemsAndFields(t *testing.T) {
	decl := map[string]any{
		"kind": "keyvalue", "title": "磁盘与资源",
		// 内建服务就是这样传的：Go 的 []map[string]any，不是 []any
		"items": []map[string]any{
			{"label": "disk", "value": "42% 已用"},
			{"label": "mem", "value": "31%", "tone": "ok"},
		},
		"fields": []map[string]any{
			{"key": "schedule.main", "label": "cron", "type": "text", "value": "*/5 * * * *"},
		},
		"bars": []float64{1, 2, 3},
	}
	c := buildComponent(decl)
	if len(c.Items) != 2 {
		t.Fatalf("items 被丢掉了：%#v", c.Items)
	}
	if c.Items[0].Label != "disk" || c.Items[0].Value != "42% 已用" {
		t.Fatalf("items 内容不对：%#v", c.Items[0])
	}
	if len(c.Fields) != 1 || c.Fields[0].Key != "schedule.main" {
		t.Fatalf("fields 被丢掉了：%#v", c.Fields)
	}
	if len(c.Bars) != 3 {
		t.Fatalf("bars 被丢掉了：%#v", c.Bars)
	}
}

// 同时保留 JSON 形态（外部插件走 HTTP）：两种形态都必须能解析。
func TestBuildComponentStillAcceptsJSONShapedItems(t *testing.T) {
	decl := map[string]any{
		"kind":  "list",
		"items": []any{map[string]any{"label": "abc1234", "value": "fix something"}},
	}
	c := buildComponent(decl)
	if len(c.Items) != 1 || c.Items[0].Label != "abc1234" {
		t.Fatalf("JSON 形态的 items 解析失败：%#v", c.Items)
	}
}

// 导航项同理：内建服务传 []map[string]any，只认 []any 会让导航静默消失。
func TestBuildNavAcceptsGoTypedItems(t *testing.T) {
	got := buildNav([]map[string]any{
		{"id": "overview", "label": "概览", "order": 10},
		{"id": "logs", "label": "日志", "order": 20},
	})
	if len(got) != 2 || got[0].ID != "overview" || got[1].Label != "日志" {
		t.Fatalf("导航项被丢掉了：%#v", got)
	}
	if json := buildNav([]any{map[string]any{"id": "x"}}); len(json) != 1 {
		t.Fatalf("JSON 形态的导航项解析失败：%#v", json)
	}
}

// 文件列表同样必须在直调路径上给出 []any。
//
// 这是端到端烟雾测试抓到的真实缺陷：内核把 fsops 的 []string 原样返回，
// 而调用方（sdk.FsList）按 JSON 形态断言 []any——断言失败不报错，
// 直接返回空列表。后果是"目录里明明有备份文件，面板却是空的"，
// 排查时几乎不可能怀疑到类型上。
func TestFsListReturnsAnySliceOnDirectCallPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	fs := fsops.New(1 << 20)
	if err := fs.Allow(fsops.Scope{Name: "logs", Root: dir, Write: false}); err != nil {
		t.Fatal(err)
	}
	policy := perm.DefaultPolicy()
	const subject = "test:subject"
	policy.Assign[subject] = perm.RoleOperator

	table := Register(Deps{FS: fs, Policy: policy})
	data, cerr := table["fs.list"](context.Background(), Call{
		Name:   "fs.list",
		Args:   map[string]any{"path": "logs:"},
		Caller: CallerInfo{Plugin: "t", Subject: subject},
	})
	if cerr != nil {
		t.Fatalf("fs.list 失败：%v", cerr)
	}
	items, ok := data["items"].([]any)
	if !ok {
		t.Fatalf("fs.list 必须给出 []any，实际 %T（调用方会因此拿到空列表）", data["items"])
	}
	if len(items) != 1 || items[0] != "a.log" {
		t.Fatalf("列目录结果不对：%#v", items)
	}
}
