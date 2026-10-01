package deployer

import (
	"context"
	"errors"
	"sync"
	"testing"

	"samryetha/sdk"
)

// scriptedKernel 按调用名返回预设结果，并记录调用序列。
//
// 内嵌 sdk.Kernel 是刻意的：被测代码若改回"起 shell 抓输出"的老写法，
// 会命中未实现的 Spawn/Wait/Output 而立刻 panic，而不是悄悄通过测试。
type scriptedKernel struct {
	sdk.Kernel
	mu     sync.Mutex
	calls  []string
	reply  map[string]map[string]any
	failed map[string]error
}

func (k *scriptedKernel) Call(name string, args map[string]any) (map[string]any, error) {
	k.mu.Lock()
	k.calls = append(k.calls, name)
	k.mu.Unlock()
	if err, ok := k.failed[name]; ok {
		return nil, err
	}
	if d, ok := k.reply[name]; ok {
		return d, nil
	}
	return map[string]any{}, nil
}

func (k *scriptedKernel) called(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range k.calls {
		if c == name {
			return true
		}
	}
	return false
}

// 备份列表改成走内核登记的 logs: 范围：既不再写死 /opt/Samryetha，
// 也把"过滤 + 倒序 + 截断"这段纯逻辑拎出来单独可测。
func TestPickBackupsFiltersSortsNewestFirstAndCaps(t *testing.T) {
	names := []string{
		"db-backup-20260901-030000.sql.gz",
		"db-backup-20260930-030000.sql.gz",
		"update.log",
		"db-backup-20260915-030000.sql.gz",
		"nested/",
	}
	got := pickBackups(names, 2)
	if len(got) != 2 {
		t.Fatalf("应截断到 2 条，实际 %d 条：%#v", len(got), got)
	}
	if got[0].name != "db-backup-20260930-030000.sql.gz" {
		t.Fatalf("最新的备份应排第一，实际 %q", got[0].name)
	}
	if got[1].name != "db-backup-20260915-030000.sql.gz" {
		t.Fatalf("第二条应是次新的备份，实际 %q", got[1].name)
	}
	if got[0].when != "2026-09-30 03:00:00" {
		t.Fatalf("时间应从文件名解出，实际 %q", got[0].when)
	}
}

func TestBackupTimeLeavesUnparsableNamesEmpty(t *testing.T) {
	if got := backupTime("db-backup-not-a-time"); got != "" {
		t.Fatalf("解不出时间时应留白（不猜），实际 %q", got)
	}
	if got := backupTime("db-backup-2026"); got != "" {
		t.Fatalf("过短的名字应留白，实际 %q", got)
	}
}

// 回归：备份面板必须经 logs: 范围读取，不得再出现写死的绝对路径。
func TestBackupListReadsThroughLogsScope(t *testing.T) {
	k := &scriptedKernel{reply: map[string]map[string]any{
		"fs.list": {"items": []any{"db-backup-20260930-030000.sql.gz", "update.log"}},
	}}
	s := New(k, nil)

	got := s.backupList(context.Background())
	if !k.called("fs.list") {
		t.Fatal("应经内核 syscall 列目录，而不是自己起进程")
	}
	if len(got) != 1 || got[0].name != "db-backup-20260930-030000.sql.gz" {
		t.Fatalf("应只保留备份文件，实际 %#v", got)
	}
}

// 操作审计的数据源必须是事件历史。
//
// 这里此前 tail 的是一个旧更新器写的日志文件（最后一次写入是 4 天前），
// 面板看着有内容，展示的却是已退役目标的陈年输出。
func TestAuditListUsesEventHistoryAndNewestFirst(t *testing.T) {
	k := &scriptedKernel{reply: map[string]map[string]any{
		"event.history": {"items": []any{
			map[string]any{"ts": int64(1700000000000), "topic": "deployment.finished",
				"payload": map[string]any{"target": "main", "state": "succeeded", "to": "abcdef1234567890", "duration_ms": int64(1200)}},
			map[string]any{"ts": int64(1700000060000), "topic": "deployment.failed",
				"payload": map[string]any{"target": "dev", "state": "failed", "error": "health: timeout"}},
		}},
	}}
	s := New(k, nil)

	got := s.auditList(context.Background())
	if len(got) != 2 {
		t.Fatalf("应得到 2 行审计，实际 %d 行：%#v", len(got), got)
	}
	if got[0].action != "failed" {
		t.Fatalf("最新的事件应排第一，实际 %#v", got[0])
	}
	if got[0].detail != "dev · health: timeout" {
		t.Fatalf("失败详情没带上，实际 %q", got[0].detail)
	}
	if got[1].action != "succeeded" || got[1].when == "" {
		t.Fatalf("成功那一行不完整：%#v", got[1])
	}
	if got[1].detail != "main → abcdef1234 (1200ms)" {
		t.Fatalf("详情应含目标、短版本与耗时，实际 %q", got[1].detail)
	}
}

// JSON 往返后时间戳会变成 float64，两种形态都必须能读。
func TestAuditRowAcceptsJSONNumberTimestamp(t *testing.T) {
	e, ok := auditRow(map[string]any{
		"ts": float64(1700000000000), "topic": "deployment.finished",
		"payload": map[string]any{"target": "main", "state": "succeeded"},
	})
	if !ok || e.when == "" {
		t.Fatalf("float64 时间戳应能解析，实际 ok=%v entry=%#v", ok, e)
	}
}

func TestAuditRowSkipsItemsWithoutPayload(t *testing.T) {
	if _, ok := auditRow(map[string]any{"ts": int64(1), "topic": "kernel.boot.ok"}); ok {
		t.Fatal("没有 payload 的事件不应进入审计面板")
	}
}

// 配置提醒改为经 status: 范围读取；范围本身按 managed_root 算出，
// 代码里不该再出现绝对路径。
func TestConfigNoticeReadsThroughStatusScope(t *testing.T) {
	k := &scriptedKernel{reply: map[string]map[string]any{
		"fs.read": {"data": "  {\"level\":\"warn\"}  "},
	}}
	s := New(k, nil)
	if got := s.configNotice(context.Background()); got != `{"level":"warn"}` {
		t.Fatalf("应去掉首尾空白后返回，实际 %q", got)
	}
}

func TestConfigNoticeEmptyWhenMissingOrEmptyObject(t *testing.T) {
	s := New(&scriptedKernel{failed: map[string]error{"fs.read": errors.New("no such file")}}, nil)
	if got := s.configNotice(context.Background()); got != "" {
		t.Fatalf("文件不存在时应为空，实际 %q", got)
	}

	s2 := New(&scriptedKernel{reply: map[string]map[string]any{"fs.read": {"data": "{}\n"}}}, nil)
	if got := s2.configNotice(context.Background()); got != "" {
		t.Fatalf("空对象不应渲染成面板，实际 %q", got)
	}
}
