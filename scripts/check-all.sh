#!/usr/bin/env bash
# 统一的健康检查入口。CI 与本地提交前都应跑它。
#
# 为什么要有这个脚本：这个项目的核心主张（内核纯净、syscall 契约一致、
# 服务声明式）都无法靠"看起来对"来保证。前面已经出现过多次
# "写完就以为对了、实际编译不过/语义错"的情况，所以把检查固化下来。
#
# 用法：scripts/check-all.sh
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# 硬约束工具链：精确使用 go1.26.10，禁止升级到更高小版本（1.27.x 仍在 CVE-2026-111943 影响范围）。
# 为什么不能用 go.mod 的 toolchain 行：toolchain 只是"建议"，GOTOOLCHAIN 默认 auto 时本机若有
# 更新版 Go 会照用；GOTOOLCHAIN=go1.26.10 才是"精确版本、禁止升级"的硬约束。
# Hard-constrain the toolchain: force exactly go1.26.10, no upgrade to a higher patch
# (1.27.x is still within CVE-2026-111943). Why the go.mod toolchain line is not enough: it is
# only a suggestion — GOTOOLCHAIN defaults to auto, which uses a newer local Go if present;
# GOTOOLCHAIN=go1.26.10 is the hard "exact version, no upgrade" constraint.
export GOTOOLCHAIN=go1.26.10

fail=0
step() { printf '\n=== %s ===\n' "$1"; }
ok()   { echo "  ✓ $1"; }
bad()  { echo "  ✗ $1"; fail=1; }

step "gofmt"
if [ -z "$(gofmt -l . 2>/dev/null)" ]; then ok "格式一致"; else
  gofmt -l . | sed 's/^/    /'; bad "有未格式化文件"; fi

step "go vet"
# 注意：不要写成 `go vet ./... | tee f | grep -q .`。
# 配合 `set -o pipefail`，grep -q 命中后提前退出会让 tee 收到 SIGPIPE，
# 整条管道被判为失败——于是"vet 真有报错"时脚本反而输出"通过"。
# 检查必须依据退出码本身。
if ! go vet ./... >/tmp/vet.out 2>&1; then
  bad "vet 报告问题"; sed 's/^/    /' /tmp/vet.out
else ok "vet 通过"; fi

step "go build"
if ! go build ./... >/tmp/build.out 2>&1; then
  bad "编译失败"; sed 's/^/    /' /tmp/build.out
else ok "编译通过"; fi

step "go 工具链版本断言"
# 构建后必须确认用的是 go1.26.10，防止有人在别处绕过 GOTOOLCHAIN
# Assert after build that go1.26.10 is actually in use, guarding against GOTOOLCHAIN bypass
if ! go version | grep -qF 'go1.26.10'; then
  bad "工具链不是 go1.26.10（$(go version)）"
else ok "工具链 = go1.26.10"; fi

step "syscall 契约（实现 vs 权限表）"
python3 scripts/check-syscall-contract.py || bad "契约不一致"

step "内核不变量（不含领域词汇）"
python3 scripts/check-kernel-invariant.py || bad "内核被领域概念污染"

step "deploy.yaml 可解析"
if python3 - <<'PY'
import subprocess, sys
# 用 Go 侧解析器验证，避免"人写的 YAML 与解析器能力不匹配"
r = subprocess.run(["go", "run", "./cmd/cfgcheck"], capture_output=True, text=True)
sys.exit(r.returncode)
PY
then ok "deploy.yaml 解析正常"; else bad "deploy.yaml 解析失败（或缺少 cmd/cfgcheck）"; fi

step "go test（含 -race：并发缺陷只有靠竞态检测才抓得到）"
# 为什么必须开 -race：曾有一个普通 map 被 cron 起的多个 goroutine 并发写，
# 平时编译/运行都"看起来正常"，生产上却以 fatal error 反复崩溃。
# 这类缺陷不靠 -race 就只能在线上暴露。
if ! go test -race -count=1 ./... >/tmp/test.out 2>&1; then
  bad "测试失败"; sed 's/^/    /' /tmp/test.out
else ok "测试通过（-race）"; fi

printf '\n'
if [ "$fail" -eq 0 ]; then echo "全部检查通过 ✓"; else echo "存在失败项 ✗"; fi
exit "$fail"
