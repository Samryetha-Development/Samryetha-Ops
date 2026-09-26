// Package deployer 是第一个用户态服务：把"部署/更新"表达为内核调用序列。
//
// 关键点：本服务与内核**平级**于其它未来服务（statuspage/inspector/notifier），
// 它没有特权——内核不知道"部署"是什么，只提供 proc/task/event/store。
//
// 这里刻意不写死任何项目细节：分支、构建命令、进程名、探活地址全部来自配置。
// Samryetha 只是其中一份配置（见 etc/）。
package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"samryetha/sdk"
)

// Target 描述一个可部署目标。字段全部来自配置，服务本身不知道具体值。
type Target struct {
	ID      string   `json:"id"`      // 例如 "main" / "dev"
	Branch  string   `json:"branch"`  // 来源分支
	WorkDir string   `json:"workdir"` // 检出目录
	DistDir string   `json:"distdir"` // 产物目录（用于原子切换）
	Build   []string `json:"build"`   // 构建命令序列（每项一条命令）
	Health  []Health `json:"health"`  // 探活：任一通过即视为健康
	Restart []string `json:"restart"` // 切换后重启命令
	Keep    int      `json:"keep"`    // 保留最近 N 个版本用于回滚
	Enabled bool     `json:"enabled"`
}

// Health 是一条探活规则。内核不解释它——是 deployer 通过 proc/http 实现。
type Health struct {
	Type    string `json:"type"` // http | tcp | exec
	URL     string `json:"url,omitempty"`
	Addr    string `json:"addr,omitempty"`
	Command string `json:"command,omitempty"`
	Expect  string `json:"expect,omitempty"`
}

// Result 是一次部署的结果。
type Result struct {
	Target    string   `json:"target"`
	FromRev   string   `json:"from_rev"`
	ToRev     string   `json:"to_rev"`
	State     string   `json:"state"` // succeeded | failed | rolled_back
	StartedAt int64    `json:"started_at"`
	EndedAt   int64    `json:"ended_at"`
	Steps     []string `json:"steps"`
	Error     string   `json:"error,omitempty"`
}

// Deployer 编排一次部署。所有外部动作都经 sdk.Kernel（= syscall），
// 因此它可被内建（in-proc）或外置（process/socket）运行。
type Deployer struct {
	K sdk.Kernel
}

// Deploy 执行一次部署。步骤刻意与现有 update.sh 对齐，便于迁移期比对：
//
//	校验 → 拉取 → 记录旧版本 → 构建 → 探活(预检) → 切换 → 重启 → 探活 → 记录/回滚
func (d *Deployer) Deploy(ctx context.Context, t Target) (Result, error) {
	res := Result{Target: t.ID, StartedAt: time.Now().UnixMilli()}
	rev, err := d.currentRev(ctx, t)
	res.FromRev = rev
	step := func(s string) { res.Steps = append(res.Steps, s) }

	// 1) 拉取
	step("fetch")
	if _, err := d.run(ctx, t, "git", "-C", t.WorkDir, "fetch", "origin", t.Branch, "--depth=1"); err != nil {
		return d.fail(res, "fetch: "+err.Error())
	}

	// 2) 目标版本
	step("resolve")
	toRev, err := d.output(ctx, t, "git", "-C", t.WorkDir, "rev-parse", "origin/"+t.Branch)
	if err != nil {
		return d.fail(res, "resolve: "+err.Error())
	}
	res.ToRev = strings.TrimSpace(toRev)

	// 3) 构建（到临时目录，绝不触碰运行中的产物）
	step("build")
	for _, cmd := range t.Build {
		if _, err := d.runShell(ctx, t, cmd); err != nil {
			return d.fail(res, "build: "+err.Error())
		}
	}

	// 4) 预检：新版本健康才继续（在切换前，避免坏版本上线）
	step("precheck")
	// 注：真实预检需要在新版本上起临时端口；此处先留接口，由配置的 health 决定
	for _, h := range t.Health {
		if err := d.checkHealth(ctx, t, h); err != nil {
			return d.fail(res, "precheck: "+err.Error())
		}
	}

	// 5) 切换 + 重启
	step("switch")
	for _, cmd := range t.Restart {
		if _, err := d.runShell(ctx, t, cmd); err != nil {
			return d.rollback(ctx, res, "restart: "+err.Error())
		}
	}

	// 6) 切换后探活；失败自动回滚（与 update.sh 的 main_rollback 同思路）
	step("health")
	for _, h := range t.Health {
		if err := d.checkHealth(ctx, t, h); err != nil {
			return d.rollback(ctx, res, "health: "+err.Error())
		}
	}

	res.State = "succeeded"
	res.EndedAt = time.Now().UnixMilli()
	d.emit("deployment.finished", res)
	return res, nil
}

func (d *Deployer) fail(res Result, msg string) (Result, error) {
	res.State = "failed"
	res.Error = msg
	res.EndedAt = time.Now().UnixMilli()
	d.emit("deployment.failed", res)
	return res, fmt.Errorf("%s", msg)
}

// rollback 失败时回到上一版本。与失败一样发事件，便于 UI/通知订阅。
func (d *Deployer) rollback(ctx context.Context, res Result, cause string) (Result, error) {
	res.Steps = append(res.Steps, "rollback")
	if res.FromRev != "" {
		_, _ = d.run(ctx, Target{WorkDir: targetWorkDir(res)}, "git", "reset", "--hard", res.FromRev)
	}
	res.State = "rolled_back"
	res.Error = cause
	res.EndedAt = time.Now().UnixMilli()
	d.emit("deployment.rolled_back", res)
	return res, fmt.Errorf("rolled back: %s", cause)
}

// --- 内核调用封装 ---

func (d *Deployer) run(ctx context.Context, t Target, argv ...string) (string, error) {
	pid, err := d.K.Spawn(argv, nil, t.WorkDir)
	if err != nil {
		return "", err
	}
	code, err := d.K.Wait(pid, 10*60*1000)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exit %d: %s", code, strings.Join(argv, " "))
	}
	return "", nil
}

func (d *Deployer) output(ctx context.Context, t Target, argv ...string) (string, error) {
	// 简化：真实实现通过 task.submit 捕获 stdout；此处先复用 run 语义。
	if _, err := d.run(ctx, t, argv...); err != nil {
		return "", err
	}
	return "", nil
}

func (d *Deployer) runShell(ctx context.Context, t Target, cmd string) (string, error) {
	return d.run(ctx, t, "sh", "-lc", cmd)
}

func (d *Deployer) checkHealth(ctx context.Context, t Target, h Health) error {
	switch h.Type {
	case "exec":
		_, err := d.runShell(ctx, t, h.Command)
		return err
	case "http", "tcp":
		// 真实实现：由 kmod 提供 http/tcp 驱动；内核只暴露 proc/task，
		// 因此这里经 proc.spawn 调用 curl/nc，或由内建驱动直连。
		_, err := d.runShell(ctx, t, "curl -fsS -o /dev/null --max-time 10 "+h.URL)
		return err
	}
	return nil
}

func (d *Deployer) currentRev(ctx context.Context, t Target) (string, error) {
	pid, err := d.K.Spawn([]string{"git", "-C", t.WorkDir, "rev-parse", "HEAD"}, nil, t.WorkDir)
	if err != nil {
		return "", err
	}
	_, _ = d.K.Wait(pid, 30_000)
	return "", nil // 简化：真实实现读取 stdout
}

func (d *Deployer) emit(topic string, res Result) {
	b, _ := json.Marshal(res)
	_, _ = d.K.Emit(topic, map[string]any{"result": string(b)})
}

func targetWorkDir(res Result) string { return res.Target }
