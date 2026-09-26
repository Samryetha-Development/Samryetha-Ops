// Package cron 实现内核的定时调度：5 段 cron 表达式 → 任务提交。
//
// 内核不关心任务内容（部署、体检、备份都是同一个原语），只负责"到点触发"。
// 实现为最小可用集：支持 * / 数字 / 逗号列表 / 步长(*/n) / 区间(a-b)。
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scheduler 管理定时任务。
type Scheduler struct {
	mu   sync.RWMutex
	jobs map[string]*job
	seq  int
	stop chan struct{}
}

type job struct {
	id     string
	name   string
	spec   string
	fields [5]matcher
	fn     func()
	last   time.Time
}

type matcher struct {
	any    bool
	values map[int]bool
}

func (m matcher) match(v int) bool {
	if m.any {
		return true
	}
	return m.values[v]
}

func New() *Scheduler {
	s := &Scheduler{jobs: map[string]*job{}, stop: make(chan struct{})}
	go s.loop()
	return s
}

// Add 注册一个定时任务。返回 jobID。
func (s *Scheduler) Add(name, spec string, fn func()) (string, error) {
	fields, err := parse(spec)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("cron_%d", s.seq)
	s.jobs[id] = &job{id: id, name: name, spec: spec, fields: fields, fn: fn}
	s.mu.Unlock()
	return id, nil
}

// SetHook 给已注册的任务绑定实际动作。
//
// 为什么分开：cron 由**内核**注册（内核负责到点触发），
// 但"到点做什么"属于**服务**。两者通过名字解耦，
// 服务可在任意时刻替换动作而不必重新注册调度。
func (s *Scheduler) SetHook(name string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.name == name {
			j.fn = fn
		}
	}
}

// Remove 删除任务。
func (s *Scheduler) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.jobs[id]
	delete(s.jobs, id)
	return ok
}

// List 返回任务清单。
func (s *Scheduler) List() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]any, 0, len(s.jobs))
	for _, j := range s.jobs {
		m := map[string]any{"id": j.id, "name": j.name, "spec": j.spec}
		if !j.last.IsZero() {
			m["last_run"] = j.last.UnixMilli()
		}
		out = append(out, m)
	}
	return out
}

// Close 停止调度。
func (s *Scheduler) Close() { close(s.stop) }

// loop 每 30 秒检查一次（分钟内精度，足够部署场景）。
func (s *Scheduler) loop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.tick(now)
		}
	}
}

func (s *Scheduler) tick(now time.Time) {
	s.mu.Lock()
	var due []*job
	for _, j := range s.jobs {
		if j.last.Truncate(time.Minute).Equal(now.Truncate(time.Minute)) {
			continue // 本分钟已跑过
		}
		if j.fields[0].match(now.Minute()) &&
			j.fields[1].match(now.Hour()) &&
			j.fields[2].match(now.Day()) &&
			j.fields[3].match(int(now.Month())) &&
			j.fields[4].match(int(now.Weekday())) {
			j.last = now
			due = append(due, j)
		}
	}
	s.mu.Unlock()
	for _, j := range due {
		go j.fn()
	}
}

// parse 解析 5 段 cron：分 时 日 月 周。
func parse(spec string) ([5]matcher, error) {
	var out [5]matcher
	parts := strings.Fields(strings.TrimSpace(spec))
	if len(parts) != 5 {
		return out, fmt.Errorf("expected 5 fields, got %d", len(parts))
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i, p := range parts {
		m, err := parseField(p, bounds[i][0], bounds[i][1])
		if err != nil {
			return out, fmt.Errorf("field %d (%q): %w", i, p, err)
		}
		out[i] = m
	}
	return out, nil
}

func parseField(f string, lo, hi int) (matcher, error) {
	m := matcher{values: map[int]bool{}}
	if f == "*" {
		m.any = true
		return m, nil
	}
	for _, part := range strings.Split(f, ",") {
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s <= 0 {
				return m, fmt.Errorf("bad step %q", part)
			}
			step = s
			part = part[:i]
		}
		start, end := lo, hi
		if part != "*" {
			if i := strings.Index(part, "-"); i >= 0 {
				a, err1 := strconv.Atoi(part[:i])
				b, err2 := strconv.Atoi(part[i+1:])
				if err1 != nil || err2 != nil {
					return m, fmt.Errorf("bad range %q", part)
				}
				start, end = a, b
			} else {
				v, err := strconv.Atoi(part)
				if err != nil {
					return m, fmt.Errorf("bad value %q", part)
				}
				start, end = v, v
			}
		}
		if start < lo || end > hi || start > end {
			return m, fmt.Errorf("out of range %q (%d-%d)", part, lo, hi)
		}
		for v := start; v <= end; v += step {
			m.values[v] = true
		}
	}
	return m, nil
}

// Next 计算下次触发时间（供 UI 预览，避免使用者误写表达式）。
func Next(spec string, from time.Time) (time.Time, error) {
	fields, err := parse(spec)
	if err != nil {
		return time.Time{}, err
	}
	t := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if fields[0].match(t.Minute()) && fields[1].match(t.Hour()) &&
			fields[2].match(t.Day()) && fields[3].match(int(t.Month())) &&
			fields[4].match(int(t.Weekday())) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("no match within a year")
}
