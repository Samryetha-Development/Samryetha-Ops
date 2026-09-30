package deployer

import (
	"context"
	"sync"
	"testing"

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
