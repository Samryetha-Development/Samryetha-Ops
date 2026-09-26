// Package events 实现内核的事件总线：只追加、可回放、按游标续订。
//
// 内核不知道事件的含义——它只保证：有序、有 id、有信封、可 history 查询。
// 领域语义（deployment.started 之类）完全由发布者（用户态服务）定义。
package events

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// Envelope 是标准事件信封（docs/architecture.md §4.2）。
type Envelope struct {
	ID      string         `json:"id"`
	Topic   string         `json:"topic"`
	TS      int64          `json:"ts"`
	Actor   map[string]any `json:"actor,omitempty"`
	Source  string         `json:"source"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Bus 是进程内事件总线。持久化由调用方决定（内核只提供接口）。
type Bus struct {
	mu       sync.RWMutex
	seq      uint64
	ring     []Envelope // 有界回放缓冲
	capacity int
	subs     map[uint64]chan Envelope
	nextSub  uint64
	persist  func(Envelope) error // 可选：落盘/入库
}

// New 创建总线。capacity 为回放缓冲容量（超出后丢弃最旧的）。
func New(capacity int, persist func(Envelope) error) *Bus {
	if capacity <= 0 {
		capacity = 1024
	}
	return &Bus{capacity: capacity, subs: map[uint64]chan Envelope{}, persist: persist}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "evt_" + hex.EncodeToString(b[:])
}

// Emit 发布事件并立即返回信封（含 id/ts）。
// 订阅者若阻塞过久会被丢弃本次投递，而不是拖住发布者——事件流不应阻塞内核。
func (b *Bus) Emit(topic, source string, actor, payload map[string]any) Envelope {
	b.mu.Lock()
	b.seq++
	env := Envelope{ID: newID(), Topic: topic, TS: time.Now().UnixMilli(), Source: source, Actor: actor, Payload: payload}
	b.ring = append(b.ring, env)
	if len(b.ring) > b.capacity {
		b.ring = b.ring[len(b.ring)-b.capacity:]
	}
	subs := make([]chan Envelope, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	if b.persist != nil {
		_ = b.persist(env)
	}
	for _, ch := range subs {
		select {
		case ch <- env:
		default: // 订阅者跟不上：丢弃，绝不阻塞发布者
		}
	}
	return env
}

// Subscribe 订阅全部事件（调用方按 topic 过滤）。返回取消函数。
func (b *Bus) Subscribe(buffer int) (<-chan Envelope, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Envelope, buffer)
	b.mu.Lock()
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// History 按 topic（空前缀=全部）、since（毫秒，0=不限）返回事件，时间升序。
func (b *Bus) History(topic string, since int64, limit int) []Envelope {
	b.mu.RLock()
	out := make([]Envelope, 0, len(b.ring))
	for _, e := range b.ring {
		if topic != "" && !hasTopicPrefix(e.Topic, topic) {
			continue
		}
		if since > 0 && e.TS < since {
			continue
		}
		out = append(out, e)
	}
	b.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// hasTopicPrefix 支持 "deployment" 命中 "deployment.started"（点分层级前缀）。
func hasTopicPrefix(topic, prefix string) bool {
	if topic == prefix {
		return true
	}
	if len(topic) > len(prefix) && topic[:len(prefix)] == prefix && topic[len(prefix)] == '.' {
		return true
	}
	return false
}
