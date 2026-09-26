// Package health 实现探活驱动：http / tcp / exec。
//
// 三条规则对应三种真实场景：站点可达、端口在听、命令成功。
// 内核不知道"探活"是什么——驱动经 syscall 起进程或连接端口。
package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"samryetha/sdk"
	drivers "samryetha/services/drivers"
)

// HTTP 探活：GET 且期望状态码。
type HTTP struct{ K sdk.Kernel }

func NewHTTP(k sdk.Kernel) *HTTP       { return &HTTP{K: k} }
func (h *HTTP) Name() string           { return "http" }
func (h *HTTP) Capabilities() []string { return []string{"health"} }

func (h *HTTP) Check(cx *drivers.Context, spec drivers.HealthSpec) error {
	timeout := time.Duration(or(spec.Timeout, 10)) * time.Second
	// 用内核的进程原语跑 curl，而不是自建 HTTP 客户端：
	// 这样超时/信号/输出捕获全部复用内核，驱动保持薄。
	pid, err := h.K.SpawnCapture(
		[]string{"curl", "-fsS", "-o", "/dev/null", "-w", "%{http_code}",
			"--max-time", fmt.Sprint(or(spec.Timeout, 10)), spec.URL},
		nil, "")
	if err != nil {
		return err
	}
	code, err := h.K.Wait(pid, int(timeout.Milliseconds())+2000)
	out, _ := h.K.Output(pid)
	if err != nil {
		return fmt.Errorf("http check %s: %w", spec.URL, err)
	}
	if code != 0 {
		return fmt.Errorf("http check %s: curl exit %d (%s)", spec.URL, code, strings.TrimSpace(out))
	}
	got := strings.TrimSpace(out)
	if spec.Expect != "" && got != spec.Expect {
		return fmt.Errorf("http check %s: expected %s, got %s", spec.URL, spec.Expect, got)
	}
	return nil
}

// TCP 探活：端口是否可连。
type TCP struct{ K sdk.Kernel }

func NewTCP(k sdk.Kernel) *TCP        { return &TCP{K: k} }
func (t *TCP) Name() string           { return "tcp" }
func (t *TCP) Capabilities() []string { return []string{"health"} }

func (t *TCP) Check(cx *drivers.Context, spec drivers.HealthSpec) error {
	timeout := time.Duration(or(spec.Timeout, 5)) * time.Second
	conn, err := net.DialTimeout("tcp", spec.Addr, timeout)
	if err != nil {
		return fmt.Errorf("tcp check %s: %w", spec.Addr, err)
	}
	_ = conn.Close()
	return nil
}

// Exec 探活：命令退出码为 0 即健康。
type Exec struct{ K sdk.Kernel }

func NewExec(k sdk.Kernel) *Exec       { return &Exec{K: k} }
func (e *Exec) Name() string           { return "exec" }
func (e *Exec) Capabilities() []string { return []string{"health"} }

func (e *Exec) Check(cx *drivers.Context, spec drivers.HealthSpec) error {
	pid, err := e.K.SpawnCapture([]string{"sh", "-lc", spec.Command}, nil, cx.Target.WorkDir)
	if err != nil {
		return err
	}
	code, err := e.K.Wait(pid, or(spec.Timeout, 30)*1000)
	out, _ := e.K.Output(pid)
	if err != nil {
		return fmt.Errorf("exec check: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("exec check exit %d: %s", code, strings.TrimSpace(out))
	}
	return nil
}

func or(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

var _ = http.StatusOK
var _ = context.Background
