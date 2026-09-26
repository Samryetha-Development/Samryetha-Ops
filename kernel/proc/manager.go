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
	// capture 为 true 时累积输出，供 Output() 读取
	capture bool
	output  []string
	// pipes 由两个读取协程在结束时投递，Wait 等它以确保输出完整
	pipes chan struct{}
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
	return m.spawn(argv, env, cwd, false)
}

// SpawnCapture 启动进程并**捕获**其合并输出，供调用方在 Wait 后读取。
//
// 这是通用能力（任何调用方都可能需要命令输出），因此属于内核而非某个驱动：
// 内核知道"如何捕获一个进程的输出"，但不知道调用方拿它做什么。
func (m *Manager) SpawnCapture(argv []string, env map[string]string, cwd string) (int, error) {
	return m.spawn(argv, env, cwd, true)
}

func (m *Manager) spawn(argv []string, env map[string]string, cwd string, capture bool) (int, error) {
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

	p := &Proc{
		PID: cmd.Process.Pid, Argv: argv, Cwd: cwd, Started: time.Now(), cmd: cmd,
		done: make(chan struct{}), capture: capture,
		// pipes 记录输出管道读取是否结束：Wait 必须等它，否则 capture 输出可能不完整。
		// 这是真实竞态——进程退出与管道清空之间有窗口，早读会得到空输出。
		pipes: make(chan struct{}, 2),
	}
	m.mu.Lock()
	m.procs[p.PID] = p
	m.mu.Unlock()

	// 实时行输出（内核日志）；capture 模式额外累积到有界缓冲
	go func() {
		pipeLines(stdout, func(l string) {
			m.onLine(p.PID, "stdout", l)
			if capture {
				m.appendOut(p, l)
			}
		})
		p.pipes <- struct{}{}
	}()
	go func() {
		pipeLines(stderr, func(l string) {
			m.onLine(p.PID, "stderr", l)
			if capture {
				m.appendOut(p, l)
			}
		})
		p.pipes <- struct{}{}
	}()

	// 关键顺序：先等两条输出管道读到 EOF，再调用 cmd.Wait()。
	//
	// 这是 os/exec 的硬性要求：cmd.Wait() 会关闭管道，若先 Wait，
	// 读取协程可能拿不到已产生的数据（真实竞态：30 次并发捕获有 4 次空输出）。
	go func() {
		<-p.pipes // 等 stdout 读取结束
		<-p.pipes // 等 stderr 读取结束
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

// appendOut 追加捕获输出（有界，防长任务撑爆内存）。
func (m *Manager) appendOut(p *Proc, line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.output = append(p.output, line)
	if len(p.output) > 4000 {
		p.output = p.output[len(p.output)-4000:]
	}
}

// Output 返回捕获的输出（仅 SpawnCapture 启动的进程有值）。
func (m *Manager) Output(pid int) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.procs[pid]
	if !ok {
		return "", fmt.Errorf("pid %d not managed", pid)
	}
	return strings.Join(p.output, "\n"), nil
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
	// 输出完整性由上面的 goroutine 保证（先读尽再 Wait）。
	// done 关闭即意味着：管道已读尽、进程已回收、退出码已填好。
	m.mu.RLock()
	code := p.exit
	m.mu.RUnlock()
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
