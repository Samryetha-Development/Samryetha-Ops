// Package git 实现 Source 驱动：取远端、解析版本、切换、diff。
//
// 全部动作经 sdk.Kernel（= syscall）执行，驱动没有旁路——因此内核仍不需要认识 git。
// 命令输出通过内核的 proc capture 能力取回（通用原语，非驱动专属）。
package git

import (
	"fmt"
	"strings"

	"samryetha/sdk"
	drivers "samryetha/services/drivers"
)

// Git 实现 drivers.Source。
type Git struct{ K sdk.Kernel }

func New(k sdk.Kernel) *Git           { return &Git{K: k} }
func (g *Git) Name() string           { return "git" }
func (g *Git) Capabilities() []string { return []string{"source"} }

// runCapture 跑一条命令并返回合并输出。capture=true 让内核累积 stdout/stderr，
// 我们再用 proc.output 取回——内核只需"捕获输出"这一个通用能力。
func (g *Git) runCapture(cx *drivers.Context, argv ...string) (string, error) {
	pid, err := g.K.SpawnCapture(argv, nil, cx.Target.WorkDir)
	if err != nil {
		return "", err
	}
	code, err := g.K.Wait(pid, 5*60*1000)
	if err != nil {
		return "", err
	}
	out, _ := g.K.Output(pid)
	if code != 0 {
		return out, fmt.Errorf("exit %d: %s: %s", code, strings.Join(argv, " "), strings.TrimSpace(out))
	}
	return out, nil
}

// Fetch 浅取目标分支。
func (g *Git) Fetch(cx *drivers.Context) error {
	cx.Log("info", "git fetch "+cx.Target.Branch, nil)
	_, err := g.runCapture(cx, "git", "fetch", "origin", cx.Target.Branch, "--depth=1")
	if err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	return nil
}

// Resolve 返回 origin/<branch> 的 commit sha。
func (g *Git) Resolve(cx *drivers.Context) (string, error) {
	out, err := g.runCapture(cx, "git", "rev-parse", "origin/"+cx.Target.Branch)
	if err != nil {
		return "", fmt.Errorf("git resolve: %w", err)
	}
	rev := strings.TrimSpace(out)
	if rev == "" {
		return "", fmt.Errorf("git resolve: empty rev")
	}
	return rev, nil
}

// Reset 把工作树切到指定版本。
func (g *Git) Reset(cx *drivers.Context, rev string) error {
	if _, err := g.runCapture(cx, "git", "reset", "--hard", rev); err != nil {
		return fmt.Errorf("git reset: %w", err)
	}
	return nil
}

// Head 返回当前 HEAD。
func (g *Git) Head(cx *drivers.Context) (string, error) {
	out, err := g.runCapture(cx, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Changed 返回两次版本之间变更的文件。旧对象不可用（浅克隆被回收）时返回错误，
// 由上层保守处理（强制重装依赖）——这是从 update.sh 继承的经验。
func (g *Git) Changed(cx *drivers.Context, from, to string) ([]string, error) {
	if from == "" || to == "" {
		return nil, nil
	}
	if _, err := g.runCapture(cx, "git", "cat-file", "-e", from); err != nil {
		return nil, fmt.Errorf("old rev unavailable: %w", err)
	}
	out, err := g.runCapture(cx, "git", "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}
