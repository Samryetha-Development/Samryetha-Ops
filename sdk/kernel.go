// Package sdk 是插件侧调用内核的统一入口（syscall 的客户端）。
//
// 设计（docs/architecture.md §4）：插件只能通过 syscall 与内核交互。
// 两个后端实现同一张表：
//   - Inproc：内建插件直接拿内核函数（零序列化）
//   - Remote：外部插件经 JSON-RPC over stdio/UDS
//
// 插件代码只依赖本包的接口，因此同一份逻辑可内建或外置。
package sdk

import (
	"context"
	"encoding/json"
	"fmt"
)

// Kernel 是内核暴露给插件的调用面。名字与 kernel/syscall 表一一对应。
type Kernel interface {
	// 日志
	Log(level, msg string, fields map[string]any) error
	// 事件
	Emit(topic string, payload map[string]any) (string, error)
	// 存储
	StoreGet(ns, key string) (string, bool, error)
	StoreSet(ns, key, val string) error
	StoreDel(ns, key string) error
	StoreList(ns, prefix string) ([]string, error)
	// 配置
	ConfigGet(path string) (any, error)
	// 进程（内核原语，领域中立）
	Spawn(argv []string, env map[string]string, cwd string) (int, error)
	// SpawnCapture 启动并捕获合并输出，Wait 后用 Output 取回。
	SpawnCapture(argv []string, env map[string]string, cwd string) (int, error)
	Output(pid int) (string, error)
	Signal(pid int, sig string) error
	Wait(pid int, timeoutMs int) (int, error)
	List() ([]ProcInfo, error)
	// 任务
	Submit(name string, steps []Step, timeoutMs int) (string, error)
	Status(taskID string) (TaskState, error)
	Cancel(taskID string) error
}

// ProcInfo 是进程快照。
type ProcInfo struct {
	PID     int    `json:"pid"`
	Argv    string `json:"argv"`
	Running bool   `json:"running"`
	Uptime  int64  `json:"uptime_ms"`
}

// Step 是任务里的一个可执行步骤。内核只负责"按序执行、超时、可取消"，
// 不理解步骤的业务含义——这正是"内核不知道更新"的落地。
type Step struct {
	Name    string            `json:"name"`
	Argv    []string          `json:"argv"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout int               `json:"timeout_ms,omitempty"`
	// AllowFailure 表示该步骤失败不终止任务（用于清理这类 best-effort 步骤）
	AllowFailure bool `json:"allow_failure,omitempty"`
}

// TaskState 是任务状态。
type TaskState struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	State    string   `json:"state"` // pending|running|succeeded|failed|canceled
	Step     string   `json:"step,omitempty"`
	ExitCode int      `json:"exit_code"`
	Log      []string `json:"log,omitempty"`
	Err      string   `json:"err,omitempty"`
}

// --- 可选：HTTP 客户端实现，供外部插件/CLI 使用同一组语义 ---

// HTTPKernel 通过内核 HTTP API 调用 syscall（外部插件与 CLI 的通用路径）。
type HTTPKernel struct {
	Base  string // 例如 http://127.0.0.1:3030
	Token string // 认证令牌（API token 驱动时使用）
}

func (h *HTTPKernel) call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"name": name, "args": args})
	req, err := newJSONRequest(ctx, "POST", h.Base+"/api/kernel/call", body)
	if err != nil {
		return nil, err
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := doJSON(req)
	if err != nil {
		return nil, err
	}
	if ok, _ := resp["ok"].(bool); !ok {
		if e, ok := resp["error"].(map[string]any); ok {
			return nil, fmt.Errorf("%v: %v", e["code"], e["message"])
		}
		return nil, fmt.Errorf("call %s failed", name)
	}
	data, _ := resp["data"].(map[string]any)
	return data, nil
}
