// Package deployer 是内核上的部署服务：把"部署"表达为内核调用序列。
//
// 与内核平级——它没有特权。内核只提供 proc/task/event/store/fs；
// 部署的全部领域知识（分支、构建命令、进程名、探活）来自配置。
//
// 本文件实现**真实执行**：拉取 → 构建 → 预检 → 切换 → 重启 → 探活 → 回滚。
// 与旧 update.sh 的关键差异：每一步都是可观测的内核调用（进程/任务/事件），
// 而不是一段不可分割的 shell。
package deployer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"samryetha/sdk"
	drivers "samryetha/services/drivers"
)

// Service 是部署服务。
type Service struct {
	K   sdk.Kernel
	Reg *drivers.Registry

	// running 防止并发部署（同时部署两个目标会互相踩产物目录）。
	//
	// mu 是必需的，不是优化：内核对**每个到期任务各起一个 goroutine**
	// （见 kernel/cron/cron.go 的 tick：`go j.fn()`），而 main 与 dev
	// 共用 `*/5 * * * *`，会在同一 tick 并发进入 Deploy。
	// Go 的 map 不支持并发写，没有锁时运行时会直接以
	// "fatal error: concurrent map writes" 终止**整个内核进程**——
	// 表现为 systemd 反复重启、在途部署被腰斩、内存态（last_run/事件）丢失。
	mu      sync.Mutex
	running map[string]bool
}

func New(k sdk.Kernel, reg *drivers.Registry) *Service {
	return &Service{K: k, Reg: reg, running: map[string]bool{}}
}

// claim 原子地占用某目标的"部署中"名额。返回 false 表示已有部署在跑。
//
// 检查与置位必须在同一把锁内完成，否则两个并发调用可能都读到 false。
func (s *Service) claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id] {
		return false
	}
	s.running[id] = true
	return true
}

// release 释放名额（配合 defer 使用，保证任何返回路径都会释放）。
func (s *Service) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

// Plan 描述一次部署（来自 deploy.yaml 的 target）。
type Plan struct {
	ID      string
	Enabled bool // 目标级开关：false 表示不注册调度、也不出现在控制台
	Branch  string
	WorkDir string

	Source  string // 驱动名：git | release | dir
	Build   []string
	Health  []drivers.HealthSpec
	Restart []string

	// Processes 驱动：pm2 | systemd | exec
	ProcessDriver string
	ProcessNames  []string

	// Migrations 可选
	MigrationDriver string
	MigrationDetect string
	MigrationCmd    string
	BackupBefore    bool

	// Hooks
	BeforeDeploy []string
	AfterDeploy  []string

	// Marker 是部署成功后的标记文件（scope:relpath）。
	// 状态页等消费方读它来判断"已部署版本"——不写它就会显示成"待部署"，
	// 即使代码其实已经更新（这类不一致会让人误判系统状态）。
	Marker string

	// AutoDeploy 是**最终生效**的"自动部署"开关：deploy.yaml 的 schedule.enabled
	// 与配置树里的 enabled.<id>（控制台设置表单）合并之后的值。
	// 控制台用它回显开关状态——这里曾经写死 `return true`，于是无论实际如何都显示"开"。
	AutoDeploy bool

	Keep int
}

// Outcome 是一次部署的最终状态。
type Outcome struct {
	Target   string        `json:"target"`
	From     string        `json:"from"`
	To       string        `json:"to"`
	State    string        `json:"state"` // succeeded | failed | rolled_back | skipped
	Changed  []string      `json:"changed"`
	Steps    []StepOutcome `json:"steps"`
	Err      string        `json:"error,omitempty"`
	Started  int64         `json:"started_at"`
	Ended    int64         `json:"ended_at"`
	Duration int64         `json:"duration_ms"`
}

// StepOutcome 是一步的结果（供 UI 时间线与审计）。
type StepOutcome struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	MS   int64  `json:"ms"`
	Note string `json:"note,omitempty"`
}

// Deploy 执行一次完整部署。
func (s *Service) Deploy(ctx context.Context, p Plan) Outcome {
	// 并发保护：同一目标不允许并行部署（见 Service.claim 的说明）
	if !s.claim(p.ID) {
		return Outcome{Target: p.ID, State: "skipped", Err: "a deployment for this target is already running"}
	}
	defer s.release(p.ID)

	start := time.Now()
	out := Outcome{Target: p.ID, Started: start.UnixMilli()}
	cx := &drivers.Context{
		Ctx: ctx, K: s.K,
		Target: drivers.TargetRef{ID: p.ID, WorkDir: p.WorkDir, Branch: p.Branch},
		Log: func(level, msg string, f map[string]any) {
			_ = s.K.Log(level, "[deployer] "+msg, f)
		},
		Emit: func(topic string, payload map[string]any) { _, _ = s.K.Emit(topic, payload) },
	}

	step := func(name string, fn func() error) bool {
		t0 := time.Now()
		err := fn()
		so := StepOutcome{Name: name, OK: err == nil, MS: time.Since(t0).Milliseconds()}
		if err != nil {
			so.Note = err.Error()
		}
		out.Steps = append(out.Steps, so)
		if err == nil {
			cx.Log("info", "step ok: "+name, nil)
		}
		return err == nil
	}

	src, ok := s.Reg.Source(p.Source)
	if !ok {
		return s.finish(out, start, "failed", fmt.Sprintf("source driver %q not registered", p.Source))
	}

	// 记录起始版本（回滚要用）
	if head, ok := s.Reg.Source("git"); ok && p.Source == "git" {
		if g, isGit := head.(interface {
			Head(*drivers.Context) (string, error)
		}); isGit {
			if rev, err := g.Head(cx); err == nil {
				out.From = rev
			}
		}
	}

	// 1) 拉取
	if !step("fetch", func() error { return src.Fetch(cx) }) {
		return s.finish(out, start, "failed", "fetch failed")
	}

	// 2) 解析目标版本
	var toRev string
	if !step("resolve", func() error {
		rev, err := src.Resolve(cx)
		toRev = rev
		return err
	}) {
		return s.finish(out, start, "failed", "resolve failed")
	}
	out.To = toRev

	// 2.5) 没有新提交就不部署——**必须在 before_deploy 之前判断**。
	//
	// 为什么：目标是被 cron 定期"检查"的（dev/main 都是每 5 分钟一轮），而流水线里
	// 塞满了有副作用的步骤：dev 的 before_deploy 会拿主站快照覆盖 dev 数据库
	// （dev 的登录会话就存在那个库里 → 每次覆盖都把所有人踢下线）、还有构建与重启。
	// 不做这个判断，这些副作用就是"每 5 分钟一次"，而不是"有新提交时一次"。
	if need, why := s.deployNeeded(cx, p, out.From, out.To); !need {
		out.State = "unchanged"
		out.Ended = time.Now().UnixMilli()
		out.Duration = out.Ended - out.Started
		cx.Log("info", fmt.Sprintf("no new revision at %s; skipping hooks, build and restart", short(out.To)), nil)
		// 刻意不写标记、不发事件：标记的时间戳是"上一次真正部署的时间"，
		// 事件流里也不该出现每 5 分钟一条的空转记录。
		return out
	} else {
		cx.Log("info", "deploying: "+why, nil)
	}

	// 0) 前置钩子（在构建/重启之前；见 deploy.yaml 里对各钩子的说明）
	for _, h := range p.BeforeDeploy {
		hook := h
		if !step("hook:"+firstWord(hook), func() error { return s.shell(cx, hook) }) {
			return s.finish(out, start, "failed", "before_deploy hook failed")
		}
	}

	// 3) 变更文件（用于判断是否需要迁移/重装依赖）
	if !step("diff", func() error {
		if out.From == "" {
			return nil
		}
		changed, err := src.Changed(cx, out.From, out.To)
		out.Changed = changed
		if err != nil {
			// 旧对象不可用（浅克隆）不是致命错误：保守起见标记"全部变更"
			cx.Log("warn", "diff unavailable, assuming all changed: "+err.Error(), nil)
			out.Changed = []string{"*"}
			return nil
		}
		return nil
	}) {
		return s.finish(out, start, "failed", "diff failed")
	}

	// 迁移配置来自该目标的 Plan，必须随调用传给驱动（见 drivers.MigrationSpec）
	spec := drivers.MigrationSpec{
		Detect:       p.MigrationDetect,
		Command:      p.MigrationCmd,
		BackupBefore: p.BackupBefore,
	}

	// 4) 迁移前备份（可选）
	if p.BackupBefore && p.MigrationDriver != "" {
		m, ok := s.Reg.Migrations(p.MigrationDriver)
		if !ok {
			// 以前这里是 `if ok`：驱动没注册时静默跳过，表现为"配置了迁移却从不执行"。
			// 配置与能力不符必须显式失败——静默失效比报错危险得多。
			return s.finish(out, start, "failed",
				fmt.Sprintf("migration driver %q not registered (configured in deploy.yaml)", p.MigrationDriver))
		}
		if !step("backup", func() error {
			path, err := m.Backup(cx, spec)
			if err == nil && path != "" {
				cx.Log("info", "backup at "+path, nil)
			}
			return err
		}) {
			return s.finish(out, start, "failed", "pre-migration backup failed")
		}
	}

	// 5) 切到目标版本
	if !step("sync", func() error { return src.Reset(cx, toRev) }) {
		return s.finish(out, start, "failed", "sync to target revision failed")
	}

	// 6) 构建
	for i, cmd := range p.Build {
		command := cmd
		idx := i + 1
		if !step(fmt.Sprintf("build[%d]", idx), func() error { return s.shell(cx, command) }) {
			return s.finish(out, start, "failed",
				fmt.Sprintf("build failed at step %d: %s", idx, command))
		}
	}

	// 7) 迁移
	if p.MigrationDriver != "" {
		m, ok := s.Reg.Migrations(p.MigrationDriver)
		if !ok {
			// 同上：不再静默跳过（那会让代码与 schema 悄悄漂移）
			return s.finish(out, start, "failed",
				fmt.Sprintf("migration driver %q not registered (configured in deploy.yaml)", p.MigrationDriver))
		}
		if m.Needed(cx, spec, out.Changed) {
			if !step("migrate", func() error { return m.Apply(cx, spec) }) {
				return s.finish(out, start, "failed", "migration failed")
			}
		} else {
			out.Steps = append(out.Steps, StepOutcome{Name: "migrate", OK: true, Note: "not needed"})
		}
	}

	// 8) 重启进程
	if p.ProcessDriver != "" {
		proc, ok := s.Reg.Processes(p.ProcessDriver)
		if !ok {
			return s.finish(out, start, "failed", fmt.Sprintf("process driver %q not registered", p.ProcessDriver))
		}
		if !step("restart", func() error { return proc.Reload(cx, procRef(p)) }) {
			// 重启失败 → 回滚（这是最需要自动兜底的一步）
			return s.rollback(ctx, cx, p, src, out, start, "restart failed")
		}
	} else {
		for _, cmd := range p.Restart {
			command := cmd
			if !step("restart:"+firstWord(command), func() error { return s.shell(cx, command) }) {
				return s.rollback(ctx, cx, p, src, out, start, "restart command failed")
			}
		}
	}

	// 9) 探活（失败即回滚——这是"健康门"）
	for i, h := range p.Health {
		spec := h
		if !step(fmt.Sprintf("health[%d]", i+1), func() error { return s.checkHealth(cx, spec) }) {
			return s.rollback(ctx, cx, p, src, out, start, "health check failed after switch")
		}
	}

	// 10) 写部署标记（状态页等靠它判断已部署版本）
	if p.Marker != "" {
		marker := p.Marker
		rev := out.To
		if !step("mark", func() error { return sdk.FsWrite(s.K, marker, rev) }) {
			// 标记写失败不回滚：代码已更新且健康，只是外部视图会滞后。
			// 但要明确记录，否则会出现"实际已部署、页面显示待部署"的困惑。
			cx.Log("warn", "deployment marker write failed: "+marker, nil)
		}
	}

	// 11) 后置钩子（best-effort：失败不回滚，但要记录）
	for _, h := range p.AfterDeploy {
		hook := h
		_ = step("after:"+firstWord(hook), func() error { return s.shell(cx, hook) })
	}

	return s.finish(out, start, "succeeded", "")
}

// rollback 回到起始版本并重启，然后重新探活。
func (s *Service) rollback(ctx context.Context, cx *drivers.Context, p Plan, src drivers.Source, out Outcome, start time.Time, cause string) Outcome {
	cx.Log("warn", "rolling back: "+cause, nil)
	t0 := time.Now()
	var rbErr error
	if out.From != "" {
		rbErr = src.Reset(cx, out.From)
	}
	out.Steps = append(out.Steps, StepOutcome{
		Name: "rollback", OK: rbErr == nil, MS: time.Since(t0).Milliseconds(),
		Note: "restored " + short(out.From),
	})
	// 回滚后必须重启，否则运行的仍是坏版本
	if p.ProcessDriver != "" {
		if proc, ok := s.Reg.Processes(p.ProcessDriver); ok {
			_ = proc.Reload(cx, procRef(p))
		}
	}
	// 注意：不要在这里额外 Emit——finish() 会按 state 发出完整事件。
	// 早前这里多发了一次字段不全的事件，导致事件流里出现 state/duration 为空的条目。
	return s.finish(out, start, "rolled_back", cause)
}

func (s *Service) finish(out Outcome, start time.Time, state, errMsg string) Outcome {
	out.State = state
	out.Err = errMsg
	out.Ended = time.Now().UnixMilli()
	out.Duration = out.Ended - out.Started
	topic := "deployment.finished"
	if state == "failed" {
		topic = "deployment.failed"
	} else if state == "rolled_back" {
		topic = "deployment.rolled_back"
	} else if state == "skipped" {
		topic = "deployment.skipped"
	}
	// 把每一步的结果一起发出去：失败时能直接看到"哪一步、为什么"，
	// 否则只有一个笼统的 "build failed"，排查要靠翻日志。
	steps := make([]map[string]any, 0, len(out.Steps))
	for _, st := range out.Steps {
		m := map[string]any{"name": st.Name, "ok": st.OK, "ms": st.MS}
		if st.Note != "" {
			m["note"] = st.Note
		}
		steps = append(steps, m)
	}
	_, _ = s.K.Emit(topic, map[string]any{
		"target": out.Target, "state": out.State, "from": out.From, "to": out.To,
		"duration_ms": out.Duration, "error": out.Err, "steps": steps,
	})
	_ = s.K.Log("info", fmt.Sprintf("deploy %s → %s (%dms)", out.Target, out.State, out.Duration), nil)
	_ = start
	return out
}

// shell 通过内核任务执行一条 shell 命令（内核负责超时/输出/取消）。
func (s *Service) shell(cx *drivers.Context, cmd string) error {
	taskID, err := s.K.Submit("shell", []sdk.Step{{
		Name: firstWord(cmd), Argv: []string{"sh", "-lc", cmd}, Cwd: cx.Target.WorkDir,
		Timeout: 30 * 60 * 1000,
	}}, 30*60*1000)
	if err != nil {
		return err
	}
	// 轮询任务状态直到结束（内建路径可返回通道，这里保持最小接口）
	for i := 0; i < 3600; i++ {
		st, err := s.K.Status(taskID)
		if err != nil {
			return err
		}
		switch st.State {
		case "succeeded":
			return nil
		case "failed":
			return fmt.Errorf("%s: %s", cmd, st.Err)
		case "canceled":
			return fmt.Errorf("%s: canceled", cmd)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s: timeout waiting for completion", cmd)
}

// deployNeeded 判断这一轮是否真的需要跑完整流水线。
//
// 目标是按 cron **定期检查**的（Samryetha 上 dev/main 都是每 5 分钟），而流水线里
// 有带副作用的步骤：before_deploy 钩子（dev 会拿主站快照覆盖 dev 数据库，而 dev 的
// 登录会话就存在那个库里 → 每次覆盖都把所有人踢下线）、构建、重启。
// 没有这个判断时，副作用每 5 分钟发生一次；需要的是"有新提交时"发生一次。
//
// 判据（任一成立就部署），返回的字符串用于日志说明"为什么这轮要部署"：
//   - 解析不到目标版本；
//   - 目标版本与本地版本不同（有新提交）；
//   - 部署标记与目标版本不一致（上一轮没走完，得补上）；
//   - 探活失败（实例挂了要能自愈，而不是"没变化"就永远不管）。
func (s *Service) deployNeeded(cx *drivers.Context, p Plan, from, to string) (bool, string) {
	if to == "" {
		return true, "target revision unresolved"
	}
	if from == "" {
		return true, "no local revision recorded"
	}
	if to != from {
		return true, "new revision " + short(to)
	}
	if p.Marker != "" {
		v, err := sdk.FsRead(s.K, p.Marker)
		if err != nil || trimNewline(v) != to {
			return true, "deployment marker does not match " + short(to)
		}
	}
	// 探活放在最后：它是唯一会真的起进程/发请求的一步，健康时第一次就通过。
	for _, h := range p.Health {
		if err := s.checkHealth(cx, h); err != nil {
			return true, "health check failed (" + err.Error() + ")"
		}
	}
	return false, ""
}

func (s *Service) checkHealth(cx *drivers.Context, h drivers.HealthSpec) error {
	drv, ok := s.Reg.Health(h.Type)
	if !ok {
		return fmt.Errorf("health driver %q not registered", h.Type)
	}
	// 必须重试：服务重启后需要时间就绪（实测 dev 后端启动要数秒）。立即探活会把
	// "还没起来"误判为"部署失败"并触发不必要的回滚——旧 update.sh 用 12×3s 窗口，
	// 这里保持同等语义，并允许通过 config 覆盖。
	attempts, interval := 12, 3*time.Second
	if cfg, err := s.K.ConfigGet("deployer.health_retry"); err == nil {
		if m, ok := cfg.(map[string]any); ok {
			if v, ok := m["attempts"].(float64); ok && v > 0 {
				attempts = int(v)
			}
			if v, ok := m["interval_seconds"].(float64); ok && v > 0 {
				interval = time.Duration(v) * time.Second
			}
		}
	}
	var last error
	for i := 1; i <= attempts; i++ {
		if err := drv.Check(cx, h); err == nil {
			if i > 1 {
				cx.Log("info", fmt.Sprintf("health ok after %d attempts", i), nil)
			}
			return nil
		} else {
			last = err
		}
		if i < attempts {
			time.Sleep(interval)
		}
	}
	return fmt.Errorf("after %d attempts: %w", attempts, last)
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i > 0 {
		return s[:i]
	}
	return s
}

// procRef 把某个目标的进程配置整理成驱动参数。
//
// 名字必须来自**这个目标自己的 Plan**：驱动实例是无状态的，谁也不会替谁重启进程。
// 见 drivers.ProcessRef 的说明（那里记着一次真实事故：按驱动名注册实例，
// 后注册的目标覆盖前一个，结果部署 main 去重启了 dev 的进程）。
func procRef(p Plan) drivers.ProcessRef {
	return drivers.ProcessRef{Driver: p.ProcessDriver, Names: p.ProcessNames, Commands: p.Restart}
}

func short(rev string) string {
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}

// RollbackToPrevious 把目标回滚到上一版（界面上"回滚"按钮的实现）。
//
// 与失败自动回滚的区别：这是**人工主动回滚**，因此：
//   - 从内核事件历史里找该目标上一次成功的版本作为目标
//   - 回滚后同样要重启并探活（否则运行的还是坏版本）
//   - 成功后**写标记**，否则状态页会继续显示错误的版本
func (s *Service) RollbackToPrevious(ctx context.Context, p Plan) (map[string]any, error) {
	// 人工回滚同样是"改动工作目录 + 重启 + 探活"，必须与定时部署互斥，
	// 否则两者会同时 reset/重启同一个目标。
	if !s.claim(p.ID) {
		return nil, fmt.Errorf("a deployment for %s is already running; try again after it finishes", p.ID)
	}
	defer s.release(p.ID)

	cx := &drivers.Context{
		Ctx: ctx, K: s.K,
		Target: drivers.TargetRef{ID: p.ID, WorkDir: p.WorkDir, Branch: p.Branch},
		Log:    func(level, msg string, f map[string]any) { _ = s.K.Log(level, "[rollback] "+msg, f) },
		Emit:   func(topic string, payload map[string]any) { _, _ = s.K.Emit(topic, payload) },
	}

	// 找上一次成功部署的版本
	prev, err := s.lastSuccessfulRev(p.ID)
	if err != nil {
		return nil, err
	}
	if prev == "" {
		return nil, fmt.Errorf("no previous successful revision recorded for %s", p.ID)
	}

	src, ok := s.Reg.Source(p.Source)
	if !ok {
		return nil, fmt.Errorf("source driver %q not registered", p.Source)
	}

	if err := src.Reset(cx, prev); err != nil {
		return nil, fmt.Errorf("reset to %s: %w", short(prev), err)
	}
	if p.ProcessDriver != "" {
		if proc, ok := s.Reg.Processes(p.ProcessDriver); ok {
			if err := proc.Reload(cx, procRef(p)); err != nil {
				return nil, fmt.Errorf("restart after rollback: %w", err)
			}
		}
	}
	for _, h := range p.Health {
		if err := s.checkHealth(cx, h); err != nil {
			return nil, fmt.Errorf("health after rollback: %w", err)
		}
	}
	if p.Marker != "" {
		if err := sdk.FsWrite(s.K, p.Marker, prev); err != nil {
			cx.Log("warn", "marker write after rollback failed: "+err.Error(), nil)
		}
	}
	_, _ = s.K.Emit("deployment.rolled_back", map[string]any{
		"target": p.ID, "to": prev, "reason": "manual rollback",
	})
	return map[string]any{"target": p.ID, "to": prev, "state": "rolled_back"}, nil
}

// lastSuccessfulRev 从事件历史里找该目标上一次**成功**部署的版本。
//
// 为什么用事件而不是本地文件：事件流是内核维护的、跨重启保留的真相来源；
// 另外存一份"上一版"文件会引入第二个真相来源，迟早不一致。
func (s *Service) lastSuccessfulRev(target string) (string, error) {
	ev, err := sdk.CallGeneric(s.K, "event.history", map[string]any{"topic": "deployment", "limit": 200})
	if err != nil {
		return "", err
	}
	items, _ := ev["items"].([]any)
	// 从后往前找最近一次 succeeded 的 to
	for i := len(items) - 1; i >= 0; i-- {
		m, _ := items[i].(map[string]any)
		pl, _ := m["payload"].(map[string]any)
		if pl == nil || pl["target"] != target {
			continue
		}
		if pl["state"] == "succeeded" {
			if to, _ := pl["to"].(string); to != "" {
				return to, nil
			}
		}
	}
	return "", nil
}
