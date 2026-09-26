#!/bin/bash
# dev 数据同步：把主站数据库与附件镜像到 dev 环境。
#
# 为什么保留为脚本而不是搬进 Go：
#   这段逻辑涉及 sqlite 在线快照（node:sqlite backup）、rsync 排除运行中的库文件、
#   .env 派生等一连串细节，且已在生产用了很久。重写一遍只会引入新 bug，
#   而内核的 hook 机制本来就是为"挂既有脚本"设计的。
#
# 由 deploy.yaml 的 hooks.before_deploy 调用（内核负责调度与日志）。
#
# 前置条件：主站数据库存在；dev 后端已停止（否则数据库文件被占用）。
set -euo pipefail

ROOT="${SAMRYETHA_ROOT:-/opt/Samryetha}"
DEV_ROOT="${SAMRYETHA_DEV_ROOT:-/opt/Samryetha-dev}"
DB_DIR="$ROOT/backend/data"

log() { echo "$(date '+%F %T') [dev-sync] $*"; }

# pm2 不在默认 PATH（旧 update.sh 靠导出 PATH 才行）。这里显式解析，
# 否则停止命令会静默失败——dev 后端仍在跑，数据库文件被占用，同步出来的库可能是坏的。
export PATH="/home/ubuntu/.local/bin:/home/ubuntu/.nvm/versions/node/v24.20.0/bin:/usr/local/bin:/usr/bin:/bin:$PATH"

log "停止 dev 后端（释放数据库文件）"
if ! command -v pm2 >/dev/null 2>&1; then
  log "FAIL: pm2 不可用，无法确保 dev 后端已停止（继续同步可能产生损坏的库）"
  exit 1
fi
pm2 stop samryetha-dev-backend >/dev/null 2>&1 || true
# 确认真的停了：端口不再响应
for i in 1 2 3 4 5; do
  if ! curl -sS -o /dev/null --max-time 2 http://127.0.0.1:3011/api/health 2>/dev/null; then
    break
  fi
  log "  等待 dev 后端停止（$i/5）"
  sleep 1
done
if curl -sS -o /dev/null --max-time 2 http://127.0.0.1:3011/api/health 2>/dev/null; then
  log "FAIL: dev 后端仍在运行，中止同步以避免损坏数据库"
  exit 1
fi

log "重建 dev 数据目录"
rm -rf "$DEV_ROOT/backend/data" "$DEV_ROOT/backend/uploads"
mkdir -p "$DEV_ROOT/backend/data" "$DEV_ROOT/backend/uploads"

# 镜像附带目录（backups 等），但排除正在写入的库文件
if ! rsync -a --delete \
      --exclude=app.db --exclude=app.db-wal --exclude=app.db-shm \
      "$DB_DIR/" "$DEV_ROOT/backend/data/"; then
  log "FAIL: 数据目录镜像失败"
  exit 1
fi

if [ ! -f "$DB_DIR/app.db" ]; then
  log "FAIL: 主站数据库不存在: $DB_DIR/app.db"
  exit 1
fi

# 在线快照：不锁主站库、不需要停主站服务
log "在线快照主库 → dev"
if ! node -e '
  const { DatabaseSync, backup } = require("node:sqlite");
  const src = new DatabaseSync(process.argv[1], { readOnly: true });
  backup(src, process.argv[2])
    .then(() => { src.close(); process.exit(0); })
    .catch((e) => { console.error((e && e.message) || e); process.exit(1); });
' "$DB_DIR/app.db" "$DEV_ROOT/backend/data/app.db"; then
  log "FAIL: 主库快照失败"
  exit 1
fi

rm -f "$DEV_ROOT/backend/data/app.db-wal" "$DEV_ROOT/backend/data/app.db-shm"
chmod 700 "$DEV_ROOT/backend/data"
chmod 600 "$DEV_ROOT/backend/data/app.db"

if [ -d "$ROOT/backend/uploads" ]; then
  log "镜像附件"
  rsync -a --delete "$ROOT/backend/uploads/" "$DEV_ROOT/backend/uploads/"
fi

# .env 派生：同密钥/账号（保证同步过来的数据可用），但端口与域名改 dev 的。
# OIDC 回调必须跟着 dev 域名走，否则登录会以 invalid_client 失败。
log "派生 dev 的 backend/.env"
if [ -f "$ROOT/backend/.env" ]; then
  {
    grep -vE '^(PORT|APP_ORIGIN|COOKIE_SECURE|SMTP_URL|SMTP_FROM|OIDC_REDIRECT_URI|OIDC_POST_LOGOUT_REDIRECT_URI)=' \
      "$ROOT/backend/.env" || true
    cat <<EOF

# --- dev 环境覆盖（由 dev-sync-data.sh 每次同步时重写） ---
PORT=3011
APP_ORIGIN=https://development.samryetha.com
COOKIE_SECURE=true
OIDC_REDIRECT_URI=https://development.samryetha.com/api/auth/callback
OIDC_POST_LOGOUT_REDIRECT_URI=https://development.samryetha.com/
EOF
  } > "$DEV_ROOT/backend/.env"
  chmod 600 "$DEV_ROOT/backend/.env"
  log "OK: .env 已派生"
else
  log "WARN: 主站 .env 不存在，dev 沿用原有配置"
fi

log "OK: dev 数据已重置为主站快照"
