// Package statuspage 把"状态页"实现为内核上的一个普通服务。
//
// 定位：原先它是一个独立脚本（generate.py）+ 独立静态站；现在它是内核的服务之一，
// 因此天然获得：路由挂载、定时任务、事件流、UI 插槽、权限与审计——
// 不再需要自己的 cron、日志、配置读取。
//
// 它仍然可以输出静态 HTML（供纯静态托管），但**由内核按定时任务驱动**。
package statuspage

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync/atomic"
	"time"

	"samryetha/sdk"
	drivers "samryetha/services/drivers"
)

// Service 是状态页服务。
type Service struct {
	K   sdk.Kernel
	Reg *drivers.Registry
	// running 防止重入：启动时立即探测可能与定时触发重叠，
	// 两轮同时写静态页会互相覆盖（曾观察到同一轮被写两次）。
	running atomic.Bool
	// Targets 要检查的目标（来自配置：名称 + 探活规则）
	Targets []Target
	// Output 静态页输出路径（scope:relpath），空则不写盘
	Output string
	// Generator 非空时改用外部命令生成页面（复用既有生成器）。
	// 这是过渡方案：内核负责调度与探活，渲染仍由既有实现完成；
	// 待渲染能力完整迁移后置空即可。
	Generator []string
	// Interval 自检间隔（cron 表达式）
	Schedule string
}

// Target 是一个被观测对象。
type Target struct {
	ID   string
	Name string
	URL  string
	// 附加探活：端口/命令
	TCP string
	Cmd string
}

// Result 是一次探测结果。
type Result struct {
	Target string    `json:"target"`
	Name   string    `json:"name"`
	OK     bool      `json:"ok"`
	Detail string    `json:"detail"`
	MS     int64     `json:"ms"`
	At     time.Time `json:"at"`
}

// Start 注册定时任务与路由，并做一次立即探测（状态页不应冷启动为空）。
func (s *Service) Start(ctx context.Context) error {
	if s.Schedule == "" {
		s.Schedule = "*/1 * * * *" // 默认每分钟
	}
	// 注册定时任务**并绑定探测动作**。
	//
	// 早前只注册未绑定，导致"到点触发却什么都不做"的静默空转——
	// 这类问题在日志里几乎看不出来，所以统一用 BindCron 一步完成。
	if _, err := sdk.BindCron(s.K, s.Schedule, "statuspage", func() {
		s.RunOnce(context.Background())
	}); err != nil {
		return fmt.Errorf("register schedule: %w", err)
	}
	// 路由：/status.json 供外部消费；页面本体由外壳插槽呈现
	if err := sdk.MountRoute(s.K, "GET", "/status.json", true); err != nil {
		return fmt.Errorf("mount route: %w", err)
	}
	// 立即探一次
	go s.RunOnce(ctx)
	// 声明 UI：概览卡片 + 日志源
	s.declareUI()
	return nil
}

// RunOnce 执行一轮探测，发布事件并（可选）写静态页。
//
// 重入保护：未获取到锁时返回 nil（调用方视为"本轮跳过"），
// 而不是排队堆积——状态页场景下，跳过比排队更符合预期。
func (s *Service) RunOnce(ctx context.Context) []Result {
	if !s.running.CompareAndSwap(false, true) {
		s.K.Log("info", "statuspage probe skipped (previous round still running)", nil)
		return nil
	}
	defer s.running.Store(false)

	var results []Result
	for _, t := range s.Targets {
		start := time.Now()
		r := Result{Target: t.ID, Name: t.Name, At: start}
		err := s.probe(ctx, t)
		r.OK = err == nil
		r.MS = time.Since(start).Milliseconds()
		if err != nil {
			r.Detail = err.Error()
		} else {
			r.Detail = fmt.Sprintf("%dms", r.MS)
		}
		results = append(results, r)
		// 每次探测发事件：通知、审计、外部订阅都靠它
		tone := "statuspage.ok"
		if !r.OK {
			tone = "statuspage.failed"
		}
		s.K.Emit(tone, map[string]any{"target": r.Target, "name": r.Name, "detail": r.Detail, "ms": r.MS})
	}

	if len(s.Generator) > 0 {
		// 外部生成器模式：由内核跑命令产出页面（内建 statuspage 的简版渲染跳过）
		if err := s.runGenerator(ctx); err != nil {
			s.K.Log("warn", "statuspage generator failed: "+err.Error(), nil)
		}
	} else if s.Output != "" {
		if err := s.writeStatic(results); err != nil {
			s.K.Log("warn", "statuspage static write failed: "+err.Error(), nil)
		}
	}
	s.declareUIWith(results)
	return results
}

func (s *Service) probe(ctx context.Context, t Target) error {
	cx := &drivers.Context{Ctx: ctx, K: s.K, Target: drivers.TargetRef{ID: t.ID},
		Log: func(string, string, map[string]any) {}, Emit: func(string, map[string]any) {}}
	if t.URL != "" {
		h, ok := s.Reg.Health("http")
		if !ok {
			return fmt.Errorf("http driver not registered")
		}
		return h.Check(cx, drivers.HealthSpec{Type: "http", URL: t.URL, Expect: "200", Timeout: 8})
	}
	if t.TCP != "" {
		h, ok := s.Reg.Health("tcp")
		if !ok {
			return fmt.Errorf("tcp driver not registered")
		}
		return h.Check(cx, drivers.HealthSpec{Type: "tcp", Addr: t.TCP, Timeout: 5})
	}
	if t.Cmd != "" {
		h, ok := s.Reg.Health("exec")
		if !ok {
			return fmt.Errorf("exec driver not registered")
		}
		return h.Check(cx, drivers.HealthSpec{Type: "exec", Command: t.Cmd, Timeout: 20})
	}
	return fmt.Errorf("target %s has no probe", t.ID)
}

// writeStatic 生成静态页（保持与旧状态页同样的"可静态托管"能力）。
func (s *Service) writeStatic(results []Result) error {
	var b strings.Builder
	b.WriteString("<!doctype html><meta charset=utf-8><title>Status</title>")
	b.WriteString(`<style>body{font:14px system-ui;max-width:760px;margin:40px auto;padding:0 16px}
.ok{color:#15803d}.bad{color:#b91c1c}li{margin:8px 0}</style>`)
	b.WriteString("<h1>Service Status</h1><ul>")
	for _, r := range results {
		cls := "ok"
		mark := "operational"
		if !r.OK {
			cls, mark = "bad", "failed"
		}
		detail := ""
		if !r.OK {
			detail = " — " + html.EscapeString(r.Detail)
		}
		fmt.Fprintf(&b, `<li><b>%s</b>: <span class="%s">%s</span> (%.0fms)%s</li>`,
			html.EscapeString(r.Name), cls, mark, float64(r.MS), detail)
	}
	fmt.Fprintf(&b, "</ul><p>updated %s</p>", time.Now().Format(time.RFC3339))
	return sdk.FsWrite(s.K, s.Output, b.String())
}

// runGenerator 跑外部生成器（内核负责超时与输出捕获）。
func (s *Service) runGenerator(ctx context.Context) error {
	taskID, err := s.K.Submit("statuspage.generate", []sdk.Step{{
		Name: "generate", Argv: s.Generator, Timeout: 120000,
	}}, 180000)
	if err != nil {
		return err
	}
	for i := 0; i < 600; i++ {
		st, err := s.K.Status(taskID)
		if err != nil {
			return err
		}
		switch st.State {
		case "succeeded":
			return nil
		case "failed", "canceled":
			return fmt.Errorf("%s: %s", st.State, st.Err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("generator timeout")
}

func (s *Service) declareUI() { s.declareUIWith(nil) }

// declareUIWith 通过 ui.declare 呈现：外壳负责渲染，服务不产出 HTML。
func (s *Service) declareUIWith(results []Result) {
	cards := []map[string]any{}
	okCount := 0
	for _, r := range results {
		if r.OK {
			okCount++
		}
	}
	tone := "ok"
	if len(results) > 0 && okCount < len(results) {
		tone = "warn"
	}
	cards = append(cards, map[string]any{
		"kind": "stat", "title": "组件健康", "value": fmt.Sprintf("%d/%d", okCount, len(results)),
		"tone": tone, "order": 5,
	})
	for i, r := range results {
		t := "ok"
		if !r.OK {
			t = "err"
		}
		val := "operational"
		if !r.OK {
			val = "failed"
		}
		cards = append(cards, map[string]any{
			"kind": "stat", "title": r.Name, "value": val, "tone": t, "order": 10 + i,
		})
	}
	_ = sdk.DeclareUI(s.K, map[string][]map[string]any{
		"overview.cards": cards,
		"logs.sources":   {{"kind": "text", "id": "statuspage", "label": "statuspage"}},
	}, nil)
}
