// Package process 实现进程管理驱动：pm2 / systemd / exec。
//
// 三种驱动同一接口——换部署环境只需改配置里的 driver 名，服务代码零改动。
// 这正验证了"通用来自结构"：内核提供进程原语，驱动把它翻译成具体系统的命令。
package process

import (
	"fmt"
	"strings"

	"samryetha/sdk"
	drivers "samryetha/services/drivers"
)

// base 复用命令执行逻辑。
type base struct{ K sdk.Kernel }

func (b *base) run(cx *drivers.Context, argv ...string) (string, error) {
	pid, err := b.K.SpawnCapture(argv, nil, "")
	if err != nil {
		return "", err
	}
	code, err := b.K.Wait(pid, 120*1000)
	out, _ := b.K.Output(pid)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("exit %d: %s", code, strings.Join(argv, " "))
	}
	return out, nil
}

// --- pm2 ---

// PM2 通过 pm2 管理进程。
type PM2 struct {
	base
	Names []string
}

func NewPM2(k sdk.Kernel, names []string) *PM2 { return &PM2{base{k}, names} }
func (p *PM2) Name() string                    { return "pm2" }
func (p *PM2) Capabilities() []string          { return []string{"processes"} }

func (p *PM2) Reload(cx *drivers.Context) error {
	for _, n := range p.Names {
		if _, err := p.run(cx, "pm2", "reload", n); err != nil {
			return fmt.Errorf("pm2 reload %s: %w", n, err)
		}
	}
	return nil
}

func (p *PM2) Restart(cx *drivers.Context) error {
	if _, err := p.run(cx, "pm2", "restart", strings.Join(p.Names, " ")); err != nil {
		return err
	}
	return nil
}

func (p *PM2) Stop(cx *drivers.Context) error {
	for _, n := range p.Names {
		if _, err := p.run(cx, "pm2", "stop", n); err != nil {
			return err
		}
	}
	return nil
}

func (p *PM2) Status(cx *drivers.Context) ([]drivers.ProcessStatus, error) {
	out, err := p.run(cx, "pm2", "jlist")
	if err != nil {
		return nil, err
	}
	return parsePM2(out), nil
}

// --- systemd ---

// Systemd 通过 systemctl 管理单元。
type Systemd struct {
	base
	Units []string
}

func NewSystemd(k sdk.Kernel, units []string) *Systemd { return &Systemd{base{k}, units} }
func (s *Systemd) Name() string                        { return "systemd" }
func (s *Systemd) Capabilities() []string              { return []string{"processes"} }

func (s *Systemd) Reload(cx *drivers.Context) error {
	for _, u := range s.Units {
		// systemd 无 reload 语义时退化为 restart（由单元自身决定）
		if _, err := s.run(cx, "systemctl", "reload-or-restart", u); err != nil {
			return fmt.Errorf("systemctl reload-or-restart %s: %w", u, err)
		}
	}
	return nil
}

func (s *Systemd) Restart(cx *drivers.Context) error {
	for _, u := range s.Units {
		if _, err := s.run(cx, "systemctl", "restart", u); err != nil {
			return err
		}
	}
	return nil
}

func (s *Systemd) Stop(cx *drivers.Context) error {
	for _, u := range s.Units {
		if _, err := s.run(cx, "systemctl", "stop", u); err != nil {
			return err
		}
	}
	return nil
}

func (s *Systemd) Status(cx *drivers.Context) ([]drivers.ProcessStatus, error) {
	var out []drivers.ProcessStatus
	for _, u := range s.Units {
		state := "unknown"
		if o, err := s.run(cx, "systemctl", "is-active", u); err == nil {
			state = strings.TrimSpace(o)
		} else {
			state = "inactive"
		}
		out = append(out, drivers.ProcessStatus{Name: u, State: state})
	}
	return out, nil
}

// --- exec（通用兜底：任意重启命令） ---

// Exec 执行配置里的重启命令。给不适用 pm2/systemd 的环境兜底。
type Exec struct {
	base
	Commands []string
}

func NewExec(k sdk.Kernel, cmds []string) *Exec { return &Exec{base{k}, cmds} }
func (e *Exec) Name() string                    { return "exec" }
func (e *Exec) Capabilities() []string          { return []string{"processes"} }

func (e *Exec) Reload(cx *drivers.Context) error  { return e.runAll(cx) }
func (e *Exec) Restart(cx *drivers.Context) error { return e.runAll(cx) }

func (e *Exec) runAll(cx *drivers.Context) error {
	for _, c := range e.Commands {
		if _, err := e.run(cx, "sh", "-lc", c); err != nil {
			return fmt.Errorf("exec %q: %w", c, err)
		}
	}
	return nil
}

func (e *Exec) Stop(cx *drivers.Context) error { return nil }
func (e *Exec) Status(cx *drivers.Context) ([]drivers.ProcessStatus, error) {
	return []drivers.ProcessStatus{}, nil
}

// parsePM2 从 pm2 jlist 的 JSON 提取需要的字段。
// 手写解析而非引入 JSON 依赖解析器——此处仅取少数字段。
func parsePM2(raw string) []drivers.ProcessStatus {
	var out []drivers.ProcessStatus
	// 极简解析：逐对象提取 name/status/restart_time
	for _, chunk := range strings.Split(raw, `"name":"`) {
		if !strings.Contains(chunk, `"pm2_env"`) && !strings.Contains(chunk, `"status"`) {
			continue
		}
		name := chunk
		if i := strings.IndexByte(name, '"'); i > 0 {
			name = name[:i]
		}
		st := drivers.ProcessStatus{Name: name, State: "online"}
		if i := strings.Index(chunk, `"status":"`); i >= 0 {
			s := chunk[i+len(`"status":"`):]
			if j := strings.IndexByte(s, '"'); j > 0 {
				st.State = s[:j]
			}
		}
		out = append(out, st)
	}
	return out
}
