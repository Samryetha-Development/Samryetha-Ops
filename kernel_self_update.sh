#!/bin/bash
# Samryetha 内核自更新：把内核也纳入"下载 Release → 校验 → 替换 → 重启 → 健康门 → 回滚"。
#
# 为什么需要它：内核此前**没有任何自动更新路径**——CI 不构建它、服务器没有 Go、
# 仓库源码也不在服务器上，于是每次改内核都得手工 scp 二进制（本项目明确禁止的做法）。
# 现在 CI 会交叉编译内核并作为 Release 附件发布，本脚本负责安全地落地。
#
# 与 ops_self_update.sh 的关键差异（为什么不能照抄）：
#   ops 更新的是"控制台/生成器"，脚本进程与被更新的服务不同源；
#   而内核**就是运行这个脚本的东西的宿主**——重启内核会杀掉当前部署流程。
#   因此这里不把重启塞进部署流水线，而是做成独立脚本（cron/手工调用），
#   并强制"安装前预检"，因为内核一旦起不来，控制台就没了，连救都难。
#
# 用法：
#   kernel_self_update.sh            # 检查并更新
#   kernel_self_update.sh --force    # 忽略版本标记，强制重装
#   kernel_self_update.sh --status   # 只看状态
set -uo pipefail

ROOT="${KERNEL_ROOT:-/opt/Samryetha/kernel}"
OPS_REPO="${OPS_REPO:-https://github.com/Samryetha-Development/Samryetha-Ops.git}"
OPS_SLUG="$(printf '%s' "$OPS_REPO" | sed -E 's#^https?://github\.com/##; s#\.git$##')"
LOG_DIR="/opt/Samryetha/logs"
LOG_FILE="$LOG_DIR/kernel-update.log"
MARKER="$LOG_DIR/.last-deployed-kernel"
SVC="samryetha-kernel"
BIN="$ROOT/bin/kernel"
HEALTH_URL="${KERNEL_HEALTH_URL:-http://127.0.0.1:3040/healthz}"
PREFLIGHT_PORT="${KERNEL_PREFLIGHT_PORT:-3099}"
TMP=""
cleanup() { [ -n "$TMP" ] && rm -rf "$TMP"; return 0; }
trap cleanup EXIT

log() { echo "$(date '+%F %T') [kernel] $*" | tee -a "$LOG_FILE"; }
fail() { log "[fail] $*"; return 1; }

FORCE=0
[ "${1:-}" = "--force" ] && FORCE=1

latest_tag() {
  curl -fsSL --max-time 30 "https://api.github.com/repos/$OPS_SLUG/releases/latest" 2>/dev/null \
    | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("tag_name",""))
except Exception: print("")' 2>/dev/null
}

if [ "${1:-}" = "--status" ]; then
  REL="$(latest_tag)"
  LAST=""; [ -f "$MARKER" ] && LAST=$(cat "$MARKER")
  echo "内核最新发布: ${REL:-未知}"
  echo "内核已部署:   ${LAST:-无记录}"
  if [ -n "$REL" ] && [ "$REL" != "$LAST" ]; then echo "=> 有可用更新"; else echo "=> 已是最新"; fi
  echo "服务: $(systemctl is-active "$SVC" 2>/dev/null || echo unknown)"
  echo "运行二进制: $(sha256sum "$BIN" 2>/dev/null | cut -c1-16)…"
  exit 0
fi

# ---------- 版本判定（以 Release tag 为准）----------
log "==> 检查内核更新"
REL="$(latest_tag)"
if [ -z "$REL" ]; then
  fail "无法获取最新 Release（网络或 API 限流）"
  exit 1
fi
LAST=""; [ -f "$MARKER" ] && LAST=$(cat "$MARKER")
if [ "$REL" = "$LAST" ] && [ "$FORCE" != "1" ]; then
  log "[ok] 内核已是最新 ($REL)"
  exit 0
fi
log "==> 目标 $REL（当前 ${LAST:-无记录}）"

# ---------- 1) 下载 + 校验 ----------
TMP="$(mktemp -d)" || { fail "无法创建临时目录"; exit 1; }
BASE="https://github.com/$OPS_SLUG/releases/download/$REL"
NEW="$TMP/kernel"
if ! curl -fsSL --max-time 180 "$BASE/kernel" -o "$NEW"; then
  fail "下载 $REL 的 kernel 失败"; exit 1
fi
if ! curl -fsSL --max-time 60 "$BASE/kernel.sha256" -o "$TMP/kernel.sha256"; then
  fail "下载 kernel.sha256 失败"; exit 1
fi
# 附件里的 sha 文件形如 "<hash>  dist/kernel"，取第一列比对。
WANT="$(awk '{print $1}' "$TMP/kernel.sha256")"
GOT="$(sha256sum "$NEW" | awk '{print $1}')"
if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
  fail "sha256 不匹配：期望 $WANT 实际 $GOT"; exit 1
fi
# 必须是 linux/amd64 ELF，防止下到 HTML 错误页或错误架构
if command -v file >/dev/null 2>&1; then
  file "$NEW" | grep -q "ELF 64-bit.*x86-64" || { fail "下载物不是 linux/amd64 ELF"; exit 1; }
fi
chmod 755 "$NEW"
log "[..] 下载并校验通过（${GOT:0:16}…）"

# ---------- 2) 预检：新二进制必须真能起来 ----------
# 内核起不来 = 控制台没了，且没有"上一版还在跑"可依赖。所以预检不可省。
PR="$TMP/preflight-root"; mkdir -p "$PR/etc"
KERNEL_ROOT="$PR" "$NEW" -listen "127.0.0.1:$PREFLIGHT_PORT" >"$TMP/preflight.log" 2>&1 &
PPID_PRE=$!
OK=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS -o /dev/null --max-time 2 "http://127.0.0.1:$PREFLIGHT_PORT/healthz" 2>/dev/null; then OK=1; break; fi
  sleep 1
done
kill "$PPID_PRE" 2>/dev/null || true
wait "$PPID_PRE" 2>/dev/null || true
if [ "$OK" != "1" ]; then
  fail "预检失败：新二进制未能在 $PREFLIGHT_PORT 提供 /healthz"; sed 's/^/    /' "$TMP/preflight.log"
  exit 1
fi
log "[..] 预检通过（新二进制可启动并提供 /healthz）"

# ---------- 3) 备份 + 原子替换 ----------
BK="$LOG_DIR/kernel-rollback-$REL"
mkdir -p "$BK"
cp -a "$BIN" "$BK/kernel" 2>/dev/null || true
PRESENT_SHA="$(sha256sum "$BIN" 2>/dev/null | awk '{print $1}')"

install -m 0755 "$NEW" "$BIN.new" && mv -f "$BIN.new" "$BIN" || { fail "替换二进制失败"; exit 1; }
log "[..] 二进制已替换"

# ---------- 4) 重启 + 健康门 ----------
if ! sudo -n systemctl restart "$SVC"; then
  fail "systemctl restart 失败"; exit 1
fi
OK=0
for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
  if curl -fsS -o /dev/null --max-time 2 "$HEALTH_URL" 2>/dev/null; then OK=1; break; fi
  sleep 2
done
if [ "$OK" != "1" ]; then
  log "[fail] 重启后健康检查未通过，回滚到上一版"
  if [ -f "$BK/kernel" ]; then
    install -m 0755 "$BK/kernel" "$BIN"
    sudo -n systemctl restart "$SVC" || true
    sleep 2
    curl -fsS -o /dev/null "$HEALTH_URL" && log "[..] 已回滚，服务恢复" || log "[fail] 回滚后仍未健康，需人工介入"
  else
    log "[fail] 无可用回滚点（$BK 为空）"
  fi
  exit 1
fi
log "[..] 健康检查通过"

# ---------- 5) 标记 + 清理 ----------
echo "$REL" > "$MARKER"
ls -1dt "$LOG_DIR"/kernel-rollback-* 2>/dev/null | tail -n +4 | while read -r d; do rm -rf "$d"; done
log "[ok] 内核更新完成 $REL（${PRESENT_SHA:0:12} → ${GOT:0:12}）"
exit 0
