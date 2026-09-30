package migrations

import (
	"strings"
	"testing"

	"samryetha/sdk"
	drivers "samryetha/services/drivers"
)

// fakeKernel 只实现被测路径用到的进程原语。
// 嵌入 sdk.Kernel 接口：未 override 的方法在调用时会 panic，提醒测试不要误用。
type fakeKernel struct {
	sdk.Kernel
	codes []int
	outs  []string
	n     int
	argv  [][]string
}

func (f *fakeKernel) SpawnCapture(argv []string, env map[string]string, cwd string) (int, error) {
	f.argv = append(f.argv, argv)
	f.n++
	return f.n, nil
}

func (f *fakeKernel) Wait(pid, timeoutMs int) (int, error) { return f.codes[pid-1], nil }

func (f *fakeKernel) Output(pid int) (string, error) {
	if pid-1 < len(f.outs) {
		return f.outs[pid-1], nil
	}
	return "", nil
}

func testCx() *drivers.Context {
	return &drivers.Context{
		Target: drivers.TargetRef{ID: "t", WorkDir: "/tmp"},
		Log:    func(string, string, map[string]any) {},
		Emit:   func(string, map[string]any) {},
	}
}

// detect 退出 0 = 需要迁移；非 0 = 不需要。与探活"退出 0 = 成功"保持一致。
func TestNeededUsesDetectExitCode(t *testing.T) {
	c := New(&fakeKernel{codes: []int{0}})
	if !c.Needed(testCx(), drivers.MigrationSpec{Detect: "git diff | grep -q x"}, nil) {
		t.Fatal("detect 退出 0 应判定为需要迁移")
	}
	c = New(&fakeKernel{codes: []int{1}})
	if c.Needed(testCx(), drivers.MigrationSpec{Detect: "git diff | grep -q x"}, nil) {
		t.Fatal("detect 退出非 0 应判定为不需要迁移")
	}
}

// 没有 detect 时保守视为需要：漏跑迁移的代价远高于多跑一次幂等迁移。
func TestNeededDefaultsToTrueWithoutDetect(t *testing.T) {
	c := New(&fakeKernel{})
	if !c.Needed(testCx(), drivers.MigrationSpec{}, nil) {
		t.Fatal("缺少 detect 时应保守地视为需要迁移")
	}
}

func TestApplyRequiresCommand(t *testing.T) {
	c := New(&fakeKernel{})
	if err := c.Apply(testCx(), drivers.MigrationSpec{}); err == nil {
		t.Fatal("command 驱动在未配置 command 时必须报错，而不是静默成功")
	}
}

func TestApplyReportsNonZeroExit(t *testing.T) {
	c := New(&fakeKernel{codes: []int{3}, outs: []string{"boom"}})
	err := c.Apply(testCx(), drivers.MigrationSpec{Command: "exit 3"})
	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("非 0 退出必须报错且带上退出码，实际 %v", err)
	}
	c = New(&fakeKernel{codes: []int{0}})
	if err := c.Apply(testCx(), drivers.MigrationSpec{Command: "true"}); err != nil {
		t.Fatalf("退出 0 不应报错：%v", err)
	}
}

func TestBackupIsExplicitlyReported(t *testing.T) {
	// 没有备份命令时返回空路径，但必须不报错（由 deployer 记录告警）。
	c := New(&fakeKernel{})
	path, err := c.Backup(testCx(), drivers.MigrationSpec{BackupBefore: true})
	if err != nil || path != "" {
		t.Fatalf("无备份命令时应返回空路径且不报错，实际 path=%q err=%v", path, err)
	}
}

func TestDriverContract(t *testing.T) {
	var _ drivers.Migrations = New(&fakeKernel{})
	if New(&fakeKernel{}).Name() != "command" {
		t.Fatal("驱动名必须是 command（与 deploy.yaml 的 migrations.driver 对应）")
	}
}
