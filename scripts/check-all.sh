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

fail=0
step() { printf '\n=== %s ===\n' "$1"; }
ok()   { echo "  ✓ $1"; }
bad()  { echo "  ✗ $1"; fail=1; }

step "gofmt"
if [ -z "$(gofmt -l . 2>/dev/null)" ]; then ok "格式一致"; else
  gofmt -l . | sed 's/^/    /'; bad "有未格式化文件"; fi

step "go vet"
if go vet ./... 2>&1 | tee /tmp/vet.out | grep -q .; then bad "vet 报告问题"; cat /tmp/vet.out | sed 's/^/    /'; else ok "vet 通过"; fi

step "go build"
if go build ./... 2>&1 | tee /tmp/build.out | grep -q .; then bad "编译失败"; cat /tmp/build.out | sed 's/^/    /'; else ok "编译通过"; fi

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

printf '\n'
if [ "$fail" -eq 0 ]; then echo "全部检查通过 ✓"; else echo "存在失败项 ✗"; fi
exit "$fail"
