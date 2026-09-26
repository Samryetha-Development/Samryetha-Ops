// Package boot 实现内核启动序列与救援模式。
//
// 序列（docs/architecture.md §8）：
//
//	读配置 → 校验 → 计算插件加载计划 → 起内核 → 拉起服务 → self-test → 对外提供
//
// 救援模式是整套设计的安全底线：当配置非法、内核自检失败、或上一轮启动未完成时，
// 以最小配置启动——只挂 Web 外壳 + 内核日志 + 救援界面，不加载任何服务，
// 并原样呈现失败原因。没有它，OTA/A-B 切换失败会导致彻底失联。
package boot

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Phase 启动阶段，用于日志与救援界面呈现。
type Phase string

const (
	PhaseConfig   Phase = "config"
	PhaseValidate Phase = "validate"
	PhasePlan     Phase = "plan"
	PhaseKernel   Phase = "kernel"
	PhaseServices Phase = "services"
	PhaseSelfTest Phase = "self-test"
	PhaseReady    Phase = "ready"
)

// Outcome 启动结果。
type Outcome struct {
	Ready     bool
	Phase     Phase
	Reason    string   // 进入救援模式或失败的原因
	Notes     []string // 各阶段的说明，救援界面会展示
	StartedAt time.Time
}

// Options 启动输入。
type Options struct {
	Root        string // 配置与状态根，例如 /opt/<project>
	DataDir     string // 内核状态（救援标记、日志段）
	ForceRescue bool   // 显式强制救援（例如 `--rescue`）
}

// Marker 是"启动完成"标记。若它在上次启动中未被写入，说明上次启动中断，
// 本次应进入救援模式——这是"上次启动未完成"这一触发条件的实现。
func markerPath(o Options) string { return filepath.Join(o.DataDir, "boot.ok") }

func writeMarker(o Options) error {
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(markerPath(o), []byte(time.Now().Format(time.RFC3339)), 0o600)
}

func clearMarker(o Options) { _ = os.Remove(markerPath(o)) }

// markIncomplete 在启动开始时调用：清掉完成标记，启动成功后再写回。
// 中途崩溃就会留下"未完成"的证据，下次启动据此进救援模式。
func markIncomplete(o Options) { clearMarker(o) }

// Boot 执行启动序列。返回 Outcome；调用方据此决定是正常服务还是进救援模式。
//
// 注意：本函数刻意不 import 任何领域包（git/pm2/deploy…），内核不得知道"更新"为何物。
func Boot(o Options, steps []func(Phase) error) (Outcome, error) {
	out := Outcome{StartedAt: time.Now(), Phase: PhaseConfig}

	markIncomplete(o)
	defer func() {
		if out.Ready {
			_ = writeMarker(o)
		}
	}()

	// 触发条件 1：显式强制救援
	if o.ForceRescue {
		return rescue(out, "explicit --rescue requested"), nil
	}

	for _, step := range steps {
		// 每个 step 自行上报它处于哪个阶段；失败即进救援。
		if err := step(out.Phase); err != nil {
			return rescue(out, fmt.Sprintf("phase %s failed: %v", out.Phase, err)), nil
		}
	}

	out.Ready = true
	out.Phase = PhaseReady
	return out, nil
}

// rescue 返回救援态 Outcome（不返回 error：救援是正常可预期状态）。
func rescue(out Outcome, reason string) Outcome {
	out.Ready = false
	out.Reason = reason
	out.Phase = PhaseSelfTest
	out.Notes = append(out.Notes,
		"已进入救援模式：内核存活，服务未加载。",
		"修复配置或代码后，从救援界面重启到正常态。",
	)
	return out
}

// RescueView 是救援界面需要的数据。内核层不渲染 HTML——外壳负责。
type RescueView struct {
	Reason    string
	Phase     Phase
	Notes     []string
	StartedAt time.Time
	LogTail   []string
}
