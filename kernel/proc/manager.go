// Package proc 实现内核的进程原语：spawn / signal / wait / list。
//
// 内核只负责"起一个进程、看它退出、给它信号"——不知道这个进程是构建、是部署、
// 还是采集指标。领域语义属于调用它的服务。这是"内核不知道更新"的落地。
package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Proc 是一个受管进程。
type Proc struct {
	PID     int
	Argv    []string
	Cwd     string
	Started time.Time
	cmd     *exec.Cmd
	done    chan struct{}
	exit    int
	waitErr error
}

// Manager 管理内核启动的进程集合。
type Manager struct {
	mu    sync.RWMutex
	procs map[int]*Proc
	// 输出回调：把子进程 stdout/stderr 喂给内核日志（由内核注入，避免 proc 依赖 log 包）
	onLine func(pid int, stream string, line string)
}

func NewManager(onLine func(pid int, stream, line string)) *Manager {
	if onLine == nil {
		onLine = func(int, string, string) {}
	}
	return &Manager{procs: map[int]*Proc{}, onLine: onLine}
}

// Spawn 启动一个进程。返回 pid。
func (m *Manager) Spawn(argv []string, env map[string]string, cwd string) (int, error) {
	if len(argv) == 0 {
		return 0, errors.New("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	if len(env) > 0 {
		cmd.Env = flattenEnv(env)
	}
	// 独立进程组：便于整组回收，避免子进程变成孤儿
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}

	p := &Proc{PID: cmd.Process.Pid, Argv: argv, Cwd: cwd, Started: time.Now(), cmd: cmd, done: make(chan struct{})}
	m.mu.Lock()
	m.procs[p.PID] = p
	m.mu.Unlock()

	// 实时行输出（内核日志）
	go pipeLines(stdout, func(l string) { m.onLine(p.PID, "stdout", l) })
	go pipeLines(stderr, func(l string) { m.onLine(p.PID, "stderr", l) })

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				p.exit = ee.ExitCode()
			} else {
				p.exit = -1
			}
			p.waitErr = err
		}
		m.mu.Unlock()
		close(p.done)
	}()

	return p.PID, nil
}

// Signal 发送信号（名称：term/kill/int/hup）。
func (m *Manager) Signal(pid int, sig string) error {
	m.mu.RLock()
	p, ok := m.procs[pid]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("pid %d not managed", pid)
	}
	s, err := parseSignal(sig)
	if err != nil {
		return err
	}
	// 对进程组发信号，确保子进程一并收到
	return syscall.Kill(-p.PID, s)
}

// Wait 等待退出，返回退出码。超时则返回错误（不杀进程——由调用方决定）。
func (m *Manager) Wait(pid int, timeoutMs int) (int, error) {
	m.mu.RLock()
	p, ok := m.procs[pid]
	m.mu.RUnlock()
	if !ok {
		return -1, fmt.Errorf("pid %d not managed", pid)
	}
	if timeoutMs <= 0 {
		<-p.done
	} else {
		select {
		case <-p.done:
		case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
			return -1, fmt.Errorf("wait timeout after %dms", timeoutMs)
		}
	}
	m.mu.RLock()
	code := p.exit
	m.mu.RUnlock()
	// 回收记录：进程已退出，保留短时供 list 观察
	return code, nil
}

// List 返回当前受管进程快照。
func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Info, 0, len(m.procs))
	for _, p := range m.procs {
		running := true
		select {
		case <-p.done:
			running = false
		default:
		}
		out = append(out, Info{
			PID: p.PID, Argv: strings.Join(p.Argv, " "), Running: running,
			UptimeMs: time.Since(p.Started).Milliseconds(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Reap 清理已退出且超过 graceMs 的记录，避免长期运行后 map 无限增长。
func (m *Manager) Reap(graceMs int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for pid, p := range m.procs {
		select {
		case <-p.done:
			if time.Since(p.Started).Milliseconds() > int64(graceMs) {
				delete(m.procs, pid)
				n++
			}
		default:
		}
	}
	return n
}

// Info 是进程快照（与 sdk.ProcInfo 对齐）。
type Info struct {
	PID      int    `json:"pid"`
	Argv     string `json:"argv"`
	Running  bool   `json:"running"`
	UptimeMs int64  `json:"uptime_ms"`
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func parseSignal(name string) (syscall.Signal, error) {
	switch strings.ToLower(name) {
	case "term", "sigterm", "15":
		return syscall.SIGTERM, nil
	case "kill", "sigkill", "9":
		return syscall.SIGKILL, nil
	case "int", "sigint", "2":
		return syscall.SIGINT, nil
	case "hup", "sighup", "1":
		return syscall.SIGHUP, nil
	}
	return 0, fmt.Errorf("unknown signal %q", name)
}

// pipeLines 逐行读取并在结束时关闭。
func pipeLines(r interface{ Read([]byte) (int, error) }, emit func(string)) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				idx := indexByte(buf, '\n')
				if idx < 0 {
					break
				}
				emit(strings.TrimRight(string(buf[:idx]), "\r"))
				buf = buf[idx+1:]
			}
			if len(buf) > 64*1024 { // 防单行超长撑爆内存
				emit(string(buf))
				buf = buf[:0]
			}
		}
		if err != nil {
			if len(buf) > 0 {
				emit(string(buf))
			}
			return
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

var _ = context.Background // 保留 context 依赖，便于后续加入取消语义
