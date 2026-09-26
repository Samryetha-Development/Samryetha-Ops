// Package task 实现内核的任务调度：按序执行步骤、超时、可取消、可查询。
//
// 内核不理解步骤的业务含义——它只知道"这是要跑的命令、要不要允许失败"。
// 部署、体检、备份、同步数据，全都是同一个 task 原语的不同参数。
package task

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"samryetha/kernel/proc"
)

// Step 是一个步骤（与 sdk.Step 对齐）。
type Step struct {
	Name         string            `json:"name"`
	Argv         []string          `json:"argv"`
	Env          map[string]string `json:"env,omitempty"`
	Cwd          string            `json:"cwd,omitempty"`
	Timeout      int               `json:"timeout_ms,omitempty"`
	AllowFailure bool              `json:"allow_failure,omitempty"`
}

// State 是任务状态。
type State struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	State    string   `json:"state"` // pending|running|succeeded|failed|canceled
	Current  string   `json:"current,omitempty"`
	ExitCode int      `json:"exit_code"`
	Log      []string `json:"log,omitempty"`
	Err      string   `json:"err,omitempty"`
	Started  int64    `json:"started_at"`
	Ended    int64    `json:"ended_at,omitempty"`
}

// Scheduler 串行/并发地跑任务，并把过程写入日志回调。
type Scheduler struct {
	mu    sync.RWMutex
	tasks map[string]*run
	procs *proc.Manager
	log   func(taskID, level, msg string)
	// 并发上限：同一时刻最多几个任务在跑（部署类任务应设为 1，避免互相打架）
	sem chan struct{}
}

type run struct {
	st     State
	cancel chan struct{}
	done   chan struct{}
}

func NewScheduler(procs *proc.Manager, maxConcurrent int, log func(taskID, level, msg string)) *Scheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if log == nil {
		log = func(string, string, string) {}
	}
	return &Scheduler{
		tasks: map[string]*run{},
		procs: procs,
		log:   log,
		sem:   make(chan struct{}, maxConcurrent),
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "task_" + hex.EncodeToString(b[:])
}

// Submit 提交并异步执行。返回 taskID。
func (s *Scheduler) Submit(name string, steps []Step, timeoutMs int) string {
	id := newID()
	r := &run{st: State{ID: id, Name: name, State: "pending", Started: time.Now().UnixMilli()}, cancel: make(chan struct{}), done: make(chan struct{})}
	s.mu.Lock()
	s.tasks[id] = r
	s.mu.Unlock()

	go func() {
		defer close(r.done)
		s.sem <- struct{}{}        // 获取并发槽
		defer func() { <-s.sem }() // 释放

		s.set(id, func(st *State) { st.State = "running" })
		code, err := s.exec(id, steps, timeoutMs)
		s.set(id, func(st *State) {
			st.ExitCode = code
			st.Ended = time.Now().UnixMilli()
			switch {
			case err != nil:
				st.State = "failed"
				st.Err = err.Error()
			case code != 0:
				st.State = "failed"
				st.Err = fmt.Sprintf("exit code %d", code)
			default:
				st.State = "succeeded"
			}
		})
	}()
	return id
}

// exec 按序执行步骤。
func (s *Scheduler) exec(id string, steps []Step, timeoutMs int) (int, error) {
	deadline := time.Time{}
	if timeoutMs > 0 {
		deadline = time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	}
	for _, step := range steps {
		select {
		case <-s.canceled(id):
			s.append(id, "canceled before "+step.Name)
			return -1, fmt.Errorf("canceled")
		default:
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return -1, fmt.Errorf("task timeout")
		}
		s.set(id, func(st *State) { st.Current = step.Name })
		s.append(id, "▶ "+step.Name)

		pid, err := s.procs.Spawn(step.Argv, step.Env, step.Cwd)
		if err != nil {
			if step.AllowFailure {
				s.append(id, "⚠ "+step.Name+" spawn failed (allowed): "+err.Error())
				continue
			}
			return -1, fmt.Errorf("%s: spawn: %w", step.Name, err)
		}

		// 步骤超时 = min(步骤自带, 任务剩余预算)。任务级超时必须能中断**正在运行**的步骤，
		// 否则 sleep 10 这类命令会跑完才被察觉（曾是 bug：只在步骤之间检查 deadline）。
		stepTimeout := step.Timeout
		if stepTimeout == 0 {
			stepTimeout = 10 * 60 * 1000
		}
		if !deadline.IsZero() {
			remain := int(time.Until(deadline).Milliseconds())
			if remain <= 0 {
				_ = s.procs.Signal(pid, "kill")
				return -1, fmt.Errorf("task timeout")
			}
			if remain < stepTimeout {
				stepTimeout = remain
			}
		}

		code, err := s.procs.Wait(pid, stepTimeout)
		if err != nil {
			// 超时或等待失败：终止该步骤的进程组，避免留下孤儿
			_ = s.procs.Signal(pid, "kill")
			if step.AllowFailure {
				s.append(id, "⚠ "+step.Name+" timeout (allowed)")
				continue
			}
			return -1, fmt.Errorf("%s: %w", step.Name, err)
		}
		if code != 0 {
			if step.AllowFailure {
				s.append(id, fmt.Sprintf("⚠ %s exit %d (allowed)", step.Name, code))
				continue
			}
			return code, fmt.Errorf("%s: exit %d", step.Name, code)
		}
		s.append(id, "✓ "+step.Name)
	}
	return 0, nil
}

// Status 查询任务状态。
func (s *Scheduler) Status(id string) (State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.tasks[id]
	if !ok {
		return State{}, false
	}
	return r.st, true
}

// Cancel 取消任务（尽力而为：已启动的步骤会收到 kill）。
func (s *Scheduler) Cancel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("unknown task %s", id)
	}
	select {
	case <-r.cancel:
	default:
		close(r.cancel)
	}
	return nil
}

// List 返回所有任务（按开始时间倒序）。
func (s *Scheduler) List() []State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]State, 0, len(s.tasks))
	for _, r := range s.tasks {
		out = append(out, r.st)
	}
	for i := 1; i < len(out); i++ { // 插入排序，避免额外依赖
		for j := i; j > 0 && out[j].Started > out[j-1].Started; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Reap 清理已完成且超过 graceMs 的任务记录。
func (s *Scheduler) Reap(graceMs int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.tasks {
		select {
		case <-r.done:
			if time.Now().UnixMilli()-r.st.Ended > int64(graceMs) {
				delete(s.tasks, id)
				n++
			}
		default:
		}
	}
	return n
}

func (s *Scheduler) canceled(id string) <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[id].cancel
}

func (s *Scheduler) set(id string, fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.tasks[id]; ok {
		fn(&r.st)
	}
}

func (s *Scheduler) append(id, line string) {
	s.mu.Lock()
	if r, ok := s.tasks[id]; ok {
		r.st.Log = append(r.st.Log, time.Now().Format("15:04:05")+" "+line)
		if len(r.st.Log) > 2000 { // 有界，防长任务撑爆内存
			r.st.Log = r.st.Log[len(r.st.Log)-2000:]
		}
	}
	s.mu.Unlock()
	s.log(id, "info", line)
}
