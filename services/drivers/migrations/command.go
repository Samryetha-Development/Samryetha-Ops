// Package migrations 实现"命令式"迁移驱动：迁移就是跑一条命令。
//
// 为什么需要它：deploy.yaml 里 `migrations.driver: command` 早就写了，
// 但**从来没有注册过对应驱动**。deployer 的写法是
//
//	if m, ok := s.Reg.Migrations(p.MigrationDriver); ok { ... }
//
// 于是"没注册"变成"静默跳过"——迁移永不执行、也不报错。这类静默失效
// 比报错危险得多：代码与 schema 悄悄漂移，直到某天线上炸掉才发现。
//
// 本驱动让配置真正生效；同时 deployer 已改为"配置了却不支持就失败"。
package migrations

import (
	"fmt"
	"strings"
	"time"

	"samryetha/sdk"
	drivers "samryetha/services/drivers"
)

// Command 把迁移表达为一条 shell 命令。
type Command struct{ K sdk.Kernel }

func New(k sdk.Kernel) *Command { return &Command{K: k} }

func (c *Command) Name() string           { return "command" }
func (c *Command) Capabilities() []string { return []string{"migrations"} }

// Needed 判定是否需要迁移。
//
// 语义：`detect` 命令退出码 0 = 需要迁移，非 0 = 不需要。
// 这与健康检查"退出 0 = 成功"一致，避免两套相反约定。
//
// 没有 detect 时保守返回 true：多跑一次通常幂等的迁移，
// 代价远小于漏跑导致 schema 与代码不一致。
func (c *Command) Needed(cx *drivers.Context, spec drivers.MigrationSpec, changed []string) bool {
	if strings.TrimSpace(spec.Detect) == "" {
		cx.Log("info", "migrations: no detect command, assuming a migration is needed", nil)
		return true
	}
	code, _, err := c.run(cx, spec.Detect, 5*time.Minute)
	if err != nil {
		// 判定失败时不能"当作不需要"——那正是静默漏迁移的入口。
		cx.Log("warn", "migrations: detect failed, assuming needed: "+err.Error(), nil)
		return true
	}
	return code == 0
}

// Backup 在迁移前备份。
//
// 没有配置备份命令时**明确记录**"没有备份"，而不是假装做了备份——
// 后者的危险性在于出事时才发现没有可回滚的东西。
func (c *Command) Backup(cx *drivers.Context, spec drivers.MigrationSpec) (string, error) {
	cx.Log("warn", "migrations: backup requested but this driver has no backup command configured; proceeding without a backup", nil)
	return "", nil
}

// Apply 执行迁移命令。
func (c *Command) Apply(cx *drivers.Context, spec drivers.MigrationSpec) error {
	if strings.TrimSpace(spec.Command) == "" {
		return fmt.Errorf("migrations: driver \"command\" requires a non-empty command")
	}
	code, out, err := c.run(cx, spec.Command, 30*time.Minute)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("migration command exited %d: %s", code, out)
	}
	return nil
}

// run 执行一条命令并返回退出码与合并输出。
//
// 用内核的进程原语（SpawnCapture/Wait/Output）而不是自建 exec：
// 超时、信号、输出捕获全部复用内核，驱动保持"薄"。
func (c *Command) run(cx *drivers.Context, cmd string, timeout time.Duration) (int, string, error) {
	pid, err := c.K.SpawnCapture([]string{"sh", "-lc", cmd}, nil, cx.Target.WorkDir)
	if err != nil {
		return -1, "", err
	}
	code, err := c.K.Wait(pid, int(timeout.Milliseconds()))
	out, _ := c.K.Output(pid)
	if err != nil {
		return -1, strings.TrimSpace(out), err
	}
	return code, strings.TrimSpace(out), nil
}
