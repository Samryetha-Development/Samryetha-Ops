#!/bin/bash
# Samryetha Ops 自更新：把更新站自身（状态页 + 更新控制台）纳入更新流程。
#
# 由 update.sh 在 main/dev 更新之后调用。设计原则与主站一致：
#   构建到临时目录 → 校验全绿 → 原子替换 → 重启 → 健康检查 → 失败回滚。
#
# 自更新的特殊风险（与主站不同）：
#   update.sh 本身也归这个仓库管，直接覆盖会「自己改自己」，正在跑的脚本被改可能行为不一致。
#   所以：本脚本只更新 status/ 下的运行组件（生成器/体检/控制台），
#   update.sh 的更新走「.new + 人工/下次生效」的保守路径，绝不就地覆盖正在执行的脚本。
#
# 用法：
#   ops_self_update.sh            # 检查并更新
#   ops_self_update.sh --force    # 忽略版本标记
#   ops_self_update.sh --status   # 查看状态
set -uo pipefail

ROOT="${SAMRYETHA_ROOT:-/opt/Samryetha}"
OPS_REPO="${OPS_REPO:-https://github.com/Samryetha-Development/Samryetha-Ops.git}"
LOG_DIR="$ROOT/logs"
LOG_FILE="$LOG_DIR/ops-update.log"
MARKER="$LOG_DIR/.last-deployed-ops"
STATUS_DIR="$ROOT/status"
SVC="samryetha-status"
SVC_PORT="${SVC_PORT:-3030}"
TMP=""
cleanup() { [ -n "$TMP" ] && rm -rf "$TMP"; return 0; }
trap cleanup EXIT

log() { echo "$(date '+%F %T') [ops] $*" | tee -a "$LOG_FILE"; }
fail() { log "[fail] $*"; return 1; }

FORCE=0
[ "${1:-}" = "--force" ] && FORCE=1

if [ "${1:-}" = "--status" ]; then
  REMOTE=$(git ls-remote "$OPS_REPO" refs/heads/main 2>/dev/null | awk '{print $1}')
  LAST=""; [ -f "$MARKER" ] && LAST=$(cat "$MARKER")
  echo "ops 远端:   ${REMOTE:-未知}"
  echo "ops 已部署: ${LAST:-无记录}"
  [ -n "$REMOTE" ] && [ "$REMOTE" != "$LAST" ] && echo "=> 有可用更新" || echo "=> 已是最新"
  echo "服务: $(systemctl is-active "$SVC" 2>/dev/null || echo unknown)"
  exit 0
fi

log "==> 检查 ops 更新"
if ! REMOTE=$(git ls-remote "$OPS_REPO" refs/heads/main 2>>"$LOG_FILE" | awk '{print $1}'); then
  log "[fail] 无法获取 ops 仓库（网络/认证），跳过"
  exit 1
fi
if [ -z "$REMOTE" ]; then
  log "[fail] ops 仓库返回空 ref（认证或仓库异常），跳过"
  exit 1
fi

LAST=""; [ -f "$MARKER" ] && LAST=$(cat "$MARKER")
if [ "$REMOTE" = "$LAST" ] && [ "$FORCE" != "1" ]; then
  log "[ok] ops 已是最新 (${REMOTE:0:10})"
  exit 0
fi
log "==> 发现新版本 ${REMOTE:0:10}（当前 ${LAST:0:10}），开始更新"

if ! TMP=$(mktemp -d "$ROOT/.opsbuild-XXXXXX"); then
  log "[fail] 无法创建临时目录"
  exit 1
fi
if ! git clone --depth 1 --branch main "$OPS_REPO" "$TMP" >> "$LOG_FILE" 2>&1; then
  fail "克隆 ops 仓库失败"
  exit 1
fi

# ---------- 1) 校验：源码语法/编译必须先全绿，否则不动线上 ----------
log "==> 校验源码"
if ! python3 -m py_compile "$TMP/generate.py" >> "$LOG_FILE" 2>&1; then
  fail "generate.py 语法错误"
  exit 1
fi
for js in codecheck.mjs semantic-checks.mjs; do
  if [ -f "$TMP/$js" ] && ! node --check "$TMP/$js" >> "$LOG_FILE" 2>&1; then
    fail "$js 语法错误"
    exit 1
  fi
done

# 控制台：优先用 CI 预编译产物（GitHub Release / artifact），其次本机 Go，最后仓库自带
SVC_SRC="$TMP/update-service"
SVC_STAGE="$TMP/out-update-service"
mkdir -p "$SVC_STAGE"
cp "$SVC_SRC/admin.html" "$SVC_STAGE/" 2>/dev/null || true

# 1) 从 Release 附件下载（最优：服务器零 Go 依赖，且产物已在 CI 冒烟过）
# 从 Release 下载预编译产物。注意 OPS_REPO 末尾带 .git，必须剥掉，
# 否则拼出 .../Samryetha-Ops.git/releases/... 会 404。
OPS_SLUG="$(printf '%s' "$OPS_REPO" | sed -E 's#^https?://github\.com/##; s#\.git$##')"
LATEST_BIN_URL="https://github.com/${OPS_SLUG}/releases/latest/download/update-service"
download_binary() {
  local url="$1" out="$2"
  curl -fsSL --max-time 120 "$url" -o "$out" 2>>"$LOG_FILE" || return 1
  [ -s "$out" ] || return 1
  # 必须是 linux/amd64 ELF 才接受，防止下到 HTML 错误页
  if command -v file >/dev/null 2>&1; then
    file "$out" | grep -q "ELF 64-bit.*x86-64" || return 1
  fi
  chmod 755 "$out"
  return 0
}

BIN_SRC="$SVC_SRC/bin/update-service-linux-amd64"
if download_binary "$LATEST_BIN_URL" "$SVC_STAGE/update-service"; then
  log "==> 已从 Release 下载预编译 update-service"
elif command -v go >/dev/null 2>&1; then
  log "==> Release 不可用，改用本机 Go 编译"
  if ! (cd "$SVC_SRC" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w" -o "$SVC_STAGE/update-service" .) >> "$LOG_FILE" 2>&1; then
    fail "Go 编译失败"
    exit 1
  fi
elif [ -f "$BIN_SRC" ]; then
  log "==> 使用仓库预编译二进制"
  cp "$BIN_SRC" "$SVC_STAGE/update-service"
else
  fail "Release/Go/预编译产物都不可用：保留现有控制台二进制，仅更新脚本部分"
  SKIP_SVC=1
fi

# ---------- 2) 原子替换（先全部写 .new，再统一 mv） ----------
log "==> 部署"
BK="$LOG_DIR/ops-rollback-$REMOTE"
mkdir -p "$BK"
for f in generate.py codecheck.mjs semantic-checks.mjs; do
  [ -f "$STATUS_DIR/$f" ] && cp -a "$STATUS_DIR/$f" "$BK/" 2>/dev/null || true
done
[ -d "$STATUS_DIR/update-service" ] && cp -a "$STATUS_DIR/update-service" "$BK/update-service" 2>/dev/null || true

install_file() {  # $1=src $2=dst $3=mode
  cp "$1" "$2.new" && chmod "$3" "$2.new" && mv -f "$2.new" "$2"
}
for f in generate.py codecheck.mjs semantic-checks.mjs; do
  [ -f "$TMP/$f" ] && install_file "$TMP/$f" "$STATUS_DIR/$f" 755
done
if [ "${SKIP_SVC:-0}" != "1" ] && [ -f "$SVC_STAGE/update-service" ]; then
  mkdir -p "$STATUS_DIR/update-service/bin"
  # 二进制正被运行 → 先停服务再替换（避免 ETXTBSY）
  systemctl --user is-active "$SVC" >/dev/null 2>&1 || true
  sudo -n systemctl stop "$SVC" >> "$LOG_FILE" 2>&1 || true
  install_file "$SVC_STAGE/update-service" "$STATUS_DIR/update-service/bin/update-service" 755
  [ -f "$SVC_STAGE/admin.html" ] && install_file "$SVC_STAGE/admin.html" "$STATUS_DIR/update-service/admin.html" 644
  [ -f "$SVC_SRC/main.go" ] && install_file "$SVC_SRC/main.go" "$STATUS_DIR/update-service/main.go" 644
  [ -f "$SVC_SRC/go.mod" ] && install_file "$SVC_SRC/go.mod" "$STATUS_DIR/update-service/go.mod" 644
  # systemd 单元：用仓库版本作为模板，但**保留本机已有的 Environment= 行**。
  # 生产凭据（ADMIN_SUBS 等）不该进仓库；直接覆盖会把本机配置抹掉（曾导致
  # sub 白名单被清空、登录判定退回邮箱）。做法：以仓库单元为骨架，
  # 把现有单元里独有的 Environment= 行追加回去。
  if [ -f "$SVC_SRC/samryetha-status.service" ]; then
    CUR=/etc/systemd/system/samryetha-status.service
    if [ -f "$CUR" ] && ! cmp -s "$SVC_SRC/samryetha-status.service" "$CUR"; then
      MERGED="$(mktemp)"
      EXTRA="$(mktemp)"
      # 只保留「仓库模板里没有的」Environment 键，避免重复定义。
      grep -E '^Environment=[A-Za-z_][A-Za-z0-9_]*=' "$CUR" 2>/dev/null | sort -u | while read -r line; do
        key="${line#Environment=}"; key="${key%%=*}"
        grep -qE "^Environment=${key}=" "$SVC_SRC/samryetha-status.service" || printf '%s\n' "$line"
      done > "$EXTRA"
      # 以仓库模板为骨架；把本机独有项插到 [Service] 段末尾
      awk -v extra="$EXTRA" '
        BEGIN { while ((getline l < extra) > 0) ex[++n] = l }
        /^\[/ { if (insection && !done) { for (i=1;i<=n;i++) print ex[i]; done=1 } insection = ($0=="[Service]") }
        { print }
        END { if (insection && !done) for (i=1;i<=n;i++) print ex[i] }
      ' "$SVC_SRC/samryetha-status.service" > "$MERGED"
      sudo -n cp "$MERGED" "$CUR"
      rm -f "$MERGED" "$EXTRA"
      sudo -n systemctl daemon-reload
      log "[..] systemd 单元已更新（本机独有 Environment 已保留在 [Service] 段）"
    fi
  fi
  sudo -n systemctl start "$SVC" >> "$LOG_FILE" 2>&1 || true
fi

# ---------- 3) 健康检查（控制台）；失败回滚 ----------
if [ "${SKIP_SVC:-0}" != "1" ]; then
  OK=0
  for i in 1 2 3 4 5 6 7 8; do
    if curl -fsS -o /dev/null "http://127.0.0.1:$SVC_PORT/healthz" 2>/dev/null; then OK=1; break; fi
    sleep 2
  done
  if [ "$OK" != "1" ]; then
    log "[fail] 控制台健康检查未通过，回滚"
    sudo -n systemctl stop "$SVC" >> "$LOG_FILE" 2>&1 || true
    [ -d "$BK/update-service" ] && { rm -rf "$STATUS_DIR/update-service"; cp -a "$BK/update-service" "$STATUS_DIR/update-service"; }
    for f in generate.py codecheck.mjs semantic-checks.mjs; do
      [ -f "$BK/$f" ] && cp -a "$BK/$f" "$STATUS_DIR/$f"
    done
    sudo -n systemctl start "$SVC" >> "$LOG_FILE" 2>&1 || true
    fail "已回滚到上一版（回滚点 $BK）"
    exit 1
  fi
  log "[..] 控制台健康检查通过"
fi

# ---------- 4) 标记 + 清理 ----------
echo "$REMOTE" > "$MARKER"
# 保留最近 3 个回滚点
ls -1dt "$LOG_DIR"/ops-rollback-* 2>/dev/null | tail -n +4 | while read -r d; do rm -rf "$d"; done
log "[ok] ops 更新完成 ${REMOTE:0:10}"
exit 0
