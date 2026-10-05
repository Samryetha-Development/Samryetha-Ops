package deployer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"samryetha/sdk"
	drivers "samryetha/services/drivers"
)

// 这些测试针对一个真实事故：内核在生产上以
// "fatal error: concurrent map writes" 反复崩溃重启。
//
// 根因是 Service.running 这个普通 map 被并发读写在
// cron 为每个到期任务各起一个 goroutine（kernel/cron/cron.go: `go j.fn()`），
// 而 main 与 dev 共用 `*/5 * * * *`，会在同一 tick 同时进入 Deploy。
// 用 -race 运行本文件即可复现修复前的数据竞争。

// newTestService 返回一个不依赖真实内核的服务：被测分支在触达 K/Reg 之前就返回。
func newTestService() *Service { return New(nil, drivers.NewRegistry()) }

// 同一目标已有部署在跑时，Deploy 必须立即返回 skipped，
// 且不得触达 nil 的 K/Reg（命中该分支时它们本就不可用）。
func TestDeploySkipsWhenTargetAlreadyRunning(t *testing.T) {
	s := newTestService()
	if !s.claim("main") {
		t.Fatal("首次 claim 应当成功")
	}
	defer s.release("main")

	out := s.Deploy(context.Background(), Plan{ID: "main"})
	if out.State != "skipped" {
		t.Fatalf("并发部署同一目标应被跳过：state=%q err=%q", out.State, out.Err)
	}
}

// 锁只保护"谁是运行中"这个映射，不应把不同目标的部署串行化。
func TestDifferentTargetsAreNotSerialized(t *testing.T) {
	s := newTestService()
	if !s.claim("main") {
		t.Fatal("claim main 失败")
	}
	defer s.release("main")
	if !s.claim("dev") {
		t.Fatal("claim dev 失败：不同目标不应互相阻塞")
	}
	defer s.release("dev")
}

// claim 必须在**同一把锁**内完成检查与置位。
//
// 测试方式：让 n 个 goroutine 同时抢同一个目标，且**在全部抢完之前不释放**，
// 因此"检查+置位"若非原子，就会有多个 goroutine 同时看到 false 而全部抢到。
// 断言恰好一个成功。
func TestClaimIsAtomicUnderConcurrency(t *testing.T) {
	s := newTestService()
	const n = 64

	results := make([]bool, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = s.claim("main") // 期间无人释放
		}(i)
	}
	close(start)
	wg.Wait()
	defer s.release("main")

	wins := 0
	for _, ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("同一目标并发 claim 应恰好 1 次成功，实际 %d 次", wins)
	}
}

// 回归测试：对 running 映射的并发 claim/release 不得崩溃。
// 修复前该测试在 -race 下会报数据竞争，且可能直接 fatal。
func TestRunningMapIsRaceFree(t *testing.T) {
	s := newTestService()
	ids := []string{"main", "dev", "ops"}
	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		id := ids[i%len(ids)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.claim(id) {
				s.release(id)
			}
		}()
	}
	wg.Wait()
}

// nopKernel 只实现该路径会用到的 Log/Emit（其余方法嵌入接口，误用即 panic）。
type nopKernel struct{ sdk.Kernel }

func (nopKernel) Log(string, string, map[string]any) error    { return nil }
func (nopKernel) Emit(string, map[string]any) (string, error) { return "", nil }

// fakeSource 让 Deploy 快速走到迁移步骤（fetch/resolve/diff/sync 均为空实现）。
type fakeSource struct{}

func (fakeSource) Name() string                             { return "fake" }
func (fakeSource) Capabilities() []string                   { return nil }
func (fakeSource) Fetch(*drivers.Context) error             { return nil }
func (fakeSource) Resolve(*drivers.Context) (string, error) { return "rev2", nil }
func (fakeSource) Reset(*drivers.Context, string) error     { return nil }
func (fakeSource) Changed(*drivers.Context, string, string) ([]string, error) {
	return []string{"changed"}, nil
}

// 配置了迁移却没注册驱动时必须**失败**，而不是静默跳过。
//
// 这是真实事故：deployer 用 `if ok` 跳过未注册的驱动，而 command 驱动从未注册，
// 于是 deploy.yaml 里的 migrations 每次都被无声忽略——迁移永不执行也不报错。
func TestDeployFailsWhenMigrationDriverMissing(t *testing.T) {
	reg := drivers.NewRegistry()
	reg.AddSource(fakeSource{})
	s := New(nopKernel{}, reg)

	out := s.Deploy(context.Background(), Plan{ID: "t", Source: "fake", MigrationDriver: "not-registered"})
	if out.State != "failed" {
		t.Fatalf("缺失迁移驱动应使部署失败，实际 state=%q err=%q", out.State, out.Err)
	}
	if !strings.Contains(out.Err, "not registered") {
		t.Fatalf("错误应说明驱动未注册，实际 %q", out.Err)
	}
}

// deployNeeded 决定"这一轮到底要不要跑流水线"。它必须挡在 before_deploy 之前：
// dev 的钩子会拿主站快照覆盖数据库，而 dev 的会话就在那个库里——每 5 分钟覆盖一次
// 等于每 5 分钟把所有人踢下线。真实事故的背景见 service.go 里的注释。
func TestDeployNeededFollowsRevisionMarkerAndHealth(t *testing.T) {
	ctx := context.Background()
	cx := &drivers.Context{Ctx: ctx}

	t.Run("有新提交 → 要部署", func(t *testing.T) {
		s := New(nopKernel{}, drivers.NewRegistry())
		need, why := s.deployNeeded(cx, Plan{ID: "t"}, "aaaa", "bbbb")
		if !need || !strings.Contains(why, "new revision") {
			t.Fatalf("有新提交时必须部署，实际 need=%v why=%q", need, why)
		}
	})

	t.Run("版本一致 + 标记一致 + 无探活 → 跳过", func(t *testing.T) {
		k := &scriptedKernel{reply: map[string]map[string]any{"fs.read": {"data": "bbbb\n"}}}
		s := New(k, drivers.NewRegistry())
		need, why := s.deployNeeded(cx, Plan{ID: "t", Marker: "markers:.last-deployed"}, "bbbb", "bbbb")
		if need {
			t.Fatalf("没有新提交且标记一致时应跳过，实际 need=true why=%q", why)
		}
	})

	t.Run("版本一致但标记对不上（上轮没走完）→ 要部署", func(t *testing.T) {
		k := &scriptedKernel{reply: map[string]map[string]any{"fs.read": {"data": "oldrev\n"}}}
		s := New(k, drivers.NewRegistry())
		need, why := s.deployNeeded(cx, Plan{ID: "t", Marker: "markers:.last-deployed"}, "bbbb", "bbbb")
		if !need || !strings.Contains(why, "marker") {
			t.Fatalf("标记不匹配时必须补部署，实际 need=%v why=%q", need, why)
		}
	})

	t.Run("标记读不到 → 要部署", func(t *testing.T) {
		k := &scriptedKernel{failed: map[string]error{"fs.read": errors.New("no such file")}}
		s := New(k, drivers.NewRegistry())
		if need, _ := s.deployNeeded(cx, Plan{ID: "t", Marker: "markers:.last-deployed"}, "bbbb", "bbbb"); !need {
			t.Fatal("标记缺失时必须部署（否则首次部署会被跳过）")
		}
	})

	t.Run("解析不到版本 / 没有本地版本 → 要部署", func(t *testing.T) {
		s := New(nopKernel{}, drivers.NewRegistry())
		if need, _ := s.deployNeeded(cx, Plan{ID: "t"}, "aaaa", ""); !need {
			t.Fatal("目标版本为空时必须部署")
		}
		if need, _ := s.deployNeeded(cx, Plan{ID: "t"}, "", "bbbb"); !need {
			t.Fatal("没有本地版本时必须部署")
		}
	})
}
