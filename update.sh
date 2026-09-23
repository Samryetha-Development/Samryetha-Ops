#!/bin/bash
set -u

export PATH="/home/ubuntu/.local/bin:/home/ubuntu/.nvm/versions/node/v24.20.0/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

ROOT="/opt/Samryetha"
LOG_DIR="$ROOT/logs"
LOG_FILE="$LOG_DIR/update.log"
MARKER="$LOG_DIR/.last-deployed"
LOCK_FILE="$LOG_DIR/.update.lock"
REPO_URL="https://github.com/Samryetha-Development/Samryetha.git"
BRANCH="main"
BACKEND_DIST="$ROOT/backend/dist"
FRONTEND_DIST="$ROOT/frontend/dist"
DB_DIR="$ROOT/backend/data"
DB_FILES="app.db app.db-wal app.db-shm"
CZ="$ROOT/customizations/apply.py"

# ---- dev 环境（dev 分支镜像：独立进程/独立端口，数据每次同步自主站）----
DEV_ROOT="/opt/Samryetha-dev"
DEV_BRANCH="dev"
DEV_MARKER="$LOG_DIR/.last-deployed-dev"
DEV_DOMAIN="development.samryetha.com"
DEV_FE_PORT=3010
DEV_BE_PORT=3011
DEV_ROLLBACK_DIR="$LOG_DIR/rollback-dev"

TMP_BUILD=""
DEV_TMP_BUILD=""
cleanup() {
  [ -n "$TMP_BUILD" ] && rm -rf "$TMP_BUILD"
  [ -n "$DEV_TMP_BUILD" ] && rm -rf "$DEV_TMP_BUILD"
  return 0
}
trap cleanup EXIT

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  echo "$(date '+%F %T') [skip] 另一个更新进程正在运行" >> "$LOG_FILE"
  exit 0
fi

log() { echo "$(date '+%F %T') $*" | tee -a "$LOG_FILE"; }

# ---- 由 status 后台 /update 管理的参数（status/update-config.json，缺失时用默认值）----
CONFIG_FILE="$ROOT/status/update-config.json"
config_get() {  # $1=点号路径 key  $2=默认值
  [ -f "$CONFIG_FILE" ] || { printf '%s' "$2"; return 0; }
  python3 - "$CONFIG_FILE" "$1" "$2" <<'PY' 2>/dev/null || printf '%s' "$2"
import json, sys
try:
    cur = json.load(open(sys.argv[1], encoding="utf-8"))
except Exception:
    print(sys.argv[3]); raise SystemExit
for k in sys.argv[2].split("."):
    if isinstance(cur, dict) and k in cur:
        cur = cur[k]
    else:
        print(sys.argv[3]); raise SystemExit
print("true" if cur is True else "false" if cur is False else cur)
PY
}

# 告警：配置 notify.webhook 时 POST 一条消息（失败静默，不影响更新流程）
notify() {  # $1=tone(success|error)  $2=message
  local hook; hook="$(config_get notify.webhook "")"
  [ -n "$hook" ] || return 0
  python3 - "$hook" "$1" "$2" <<'PY' >/dev/null 2>&1 || true
import json, sys, urllib.request
hook, tone, msg = sys.argv[1], sys.argv[2], sys.argv[3]
body = json.dumps({"text": f"[Samryetha update][{tone}] {msg}"}).encode()
req = urllib.request.Request(hook, data=body, headers={"Content-Type": "application/json"})
try:
    urllib.request.urlopen(req, timeout=8)
except Exception:
    pass
PY
}

# 分支可由后台覆盖（默认 main/dev）
BRANCH="$(config_get main.branch "$BRANCH")"
DEV_BRANCH="$(config_get dev.branch "$DEV_BRANCH")"

# ===================== 自适应后端（Node TypeScript / Python FastAPI） =====================
# 按检出目录结构自动识别后端类型，构建/部署/回滚按类型分派，
# 使「后端整体重构」（如 dev 分支的 TS→Python 移植）也能照常自动更新，未来再变也能兼容。

# 识别后端类型：node / python / unknown
backend_type() {
  if [ -f "$1/backend/src/app/server.ts" ]; then
    echo "node"
  elif [ -f "$1/backend/pyproject.toml" ] && [ -f "$1/backend/src/samryetha/main.py" ]; then
    echo "python"
  else
    echo "unknown"
  fi
}

# 写对应后端的本地部署入口 start.sh。$1=检出根 $2=类型 $3=日志文件（空则交给 pm2 捕获）
write_start_sh() {
  local root="$1"
  [ -x "$root/backend/start.sh" ] || {
    log "[fail] 版本化 backend/start.sh 缺失或不可执行"
    return 1
  }
}

# 绑定地址已版本化在应用源码中；部署器不得再修改检出内容。
backend_bind_fix() {
  return 0
}

# 让后端 .env 始终包含 env.example 里的全部键：保留现有值（“我原来的配置”优先），
# 只补充缺失键（取 example 占位值），新代码新增的配置项也能直接启动。
ensure_env_from_example() {
  local env_file="$1/backend/.env" ex_file="$1/backend/.env.example"
  [ -f "$env_file" ] || return 0
  [ -f "$ex_file" ] || return 0
  local added=0 key val
  while IFS='=' read -r key val; do
    case "$key" in
      ""|\#*) continue ;;
    esac
    if ! grep -qE "^${key}=" "$env_file"; then
      printf '\n%s=%s\n' "$key" "$val" >> "$env_file"
      added=$((added + 1))
    fi
  done < "$ex_file"
  [ "$added" -gt 0 ] && log "[..] .env 已从 env.example 补充缺失配置项 ${added} 个（原有配置保留优先）"
}

# 后端 .env 防丢失闸门。
# .env 不在 git 里，却是 Lako OIDC 密钥等生产凭据的唯一存放处。部署路径上任何一步把
# 它整文件重生成（早期脚本的 cp .env.example .env、或 reset 撞上同路径未跟踪文件），
# 都会让主站登录静默回落成密码登录——不报错、不告警，只是 OIDC 没了。
# 所以更新前后各记一次键集合，少一个键就还原并中止本次更新。
env_guard_snapshot() {
  local env_file="$1/backend/.env"
  ENV_GUARD_KEYS=""; ENV_GUARD_COPY=""
  [ -f "$env_file" ] || return 0
  mkdir -p "$LOG_DIR"
  ENV_GUARD_COPY="$LOG_DIR/env-snapshot-$TS"
  if cp -a "$env_file" "$ENV_GUARD_COPY"; then chmod 600 "$ENV_GUARD_COPY"; else ENV_GUARD_COPY=""; fi
  ENV_GUARD_KEYS="$LOG_DIR/env-keys-$TS.before"
  grep -oaE '^[A-Za-z_][A-Za-z0-9_]*=' "$env_file" | tr -d '=' | sort -u > "$ENV_GUARD_KEYS" || true
  prune_backups "env-snapshot-*" 5
}

env_guard_check() {
  local env_file="$1/backend/.env" missing after="$LOG_DIR/env-keys-$TS.after"
  [ -n "${ENV_GUARD_KEYS:-}" ] && [ -f "${ENV_GUARD_KEYS:-}" ] || return 0
  [ -f "$env_file" ] || {
    log "[fail] 后端 .env 在本次更新中消失（更新前有 $(wc -l < "$ENV_GUARD_KEYS") 个配置项）"
    [ -n "${ENV_GUARD_COPY:-}" ] && [ -f "${ENV_GUARD_COPY:-}" ] && { cp -a "$ENV_GUARD_COPY" "$env_file"; chmod 600 "$env_file"; log "[..] 已从 $ENV_GUARD_COPY 还原"; }
    return 1
  }
  grep -oaE '^[A-Za-z_][A-Za-z0-9_]*=' "$env_file" | tr -d '=' | sort -u > "$after" || true
  missing="$(comm -23 "$ENV_GUARD_KEYS" "$after")"
  if [ -n "$missing" ]; then
    log "[fail] 本次更新抹掉了后端 .env 的配置项：$(echo "$missing" | tr '
' ' ')"
    if [ -n "${ENV_GUARD_COPY:-}" ] && [ -f "${ENV_GUARD_COPY:-}" ]; then
      cp -a "$ENV_GUARD_COPY" "$env_file"; chmod 600 "$env_file"
      log "[..] .env 已从 $ENV_GUARD_COPY 还原，本次更新中止（服务继续跑旧版本）"
    fi
    return 1
  fi
  return 0
}

# 人工配置提醒：检测「已配 SMTP_URL 但后端尚未接线邮件」等需要人工关注的配置，
# 写入 status/data/config-notice.json，状态页展示；条件消除后自动清除。
write_config_notice() {
  local root="$1" env_file="$1/backend/.env" out="$ROOT/status/data/config-notice.json"
  local msg="" smtp_configured=0 mailer_wired=0
  grep -qE "^SMTP_URL=.+" "$env_file" 2>/dev/null && smtp_configured=1
  if [ "$(backend_type "$root")" = "python" ]; then
    grep -qE "smtp_url" "$root/backend/src/samryetha/main.py" 2>/dev/null && mailer_wired=1
    if [ "$smtp_configured" = "1" ] && [ "$mailer_wired" = "0" ]; then
      msg="后端已迁移 Python：.env 已配置 SMTP_URL，但代码尚未接线邮件发送（仍为 ConsoleMailer），密码重置/通知等邮件暂不发出。请在代码接线后自动消除本提醒。"
    fi
  fi
  if [ -n "$msg" ]; then
    printf '{"ts":%s,"env":"%s","msg":%s}\n' "$(date +%s)" "$(basename "$root")" \
      "$(python3 -c "import json,sys;print(json.dumps(sys.argv[1],ensure_ascii=False))" "$msg")" > "$out"
    log "[..] 状态页已写入人工配置提醒（$out）"
  else
    rm -f "$out"
  fi
}

# 是否需要重装后端依赖（依赖文件变了，或依赖目录缺失）
backend_deps_needed() {
  local root="$1" type="$2" changed="$3"
  if [ "$type" = "python" ]; then
    echo "$changed" | grep -qE '(^|/)(pyproject\.toml|uv\.lock)$' && return 0
    [ -d "$root/backend/.venv" ] || return 0
  else
    echo "$changed" | grep -qE '(^|/)(package\.json|pnpm-lock\.yaml)$' && return 0
    [ -x "$root/backend/node_modules/.bin/tsc" ] || return 0
  fi
  return 1
}

# 安装后端依赖（python 用 uv sync，node 用 pnpm）
backend_deps() {
  if [ "$2" = "python" ]; then
    (cd "$1/backend" && uv sync) >> "$LOG_FILE" 2>&1
  else
    (cd "$1/backend" && pnpm install --ignore-scripts) >> "$LOG_FILE" 2>&1
  fi
}

# 编译/校验后端到 stage（python 无编译产物，仅导入校验确认可启动）
backend_build() {
  if [ "$2" = "python" ]; then
    (cd "$1/backend" && uv run python -c "import samryetha.main") >> "$LOG_FILE" 2>&1
  else
    (cd "$1/backend" && ./node_modules/.bin/tsc -p tsconfig.json --outDir "$3") >> "$LOG_FILE" 2>&1
  fi
}

# 校验后端构建产物就绪
backend_artifact_ok() {
  if [ "$2" = "python" ]; then
    [ -f "$1/backend/src/samryetha/main.py" ]
  else
    [ -f "$3/app/server.js" ]
  fi
}

# 部署后端产物（node 原子替换 dist；python 源码即产物，树已就位无需移动）
backend_deploy() {
  [ "$2" = "node" ] && rsync -a --delete "$3/" "$1/backend/dist/"
}

# 回滚后端（node 恢复旧 dist；python 回退 git SHA + 重装依赖 + 重套定制层）
backend_rollback() {
  local root="$1" type="$2" rb="$3" old_sha="$4"
  if [ "$type" = "python" ]; then
    git -C "$root" reset --hard "$old_sha" >> "$LOG_FILE" 2>&1
    backend_deps "$root" "$type" || true
    # git reset 会冲掉定制层改过的文件，必须重新套用，否则回滚后跑的是"无定制层"代码
    if [ -f "$CZ" ]; then
      if SAMRYETHA_ROOT="$root" python3 "$CZ" >> "$LOG_FILE" 2>&1; then
        log "[..] 回滚后定制层已重新应用"
      else
        log "[fail] 回滚后定制层应用失败（需人工检查 $CZ）"
      fi
    fi
  elif [ -d "$rb/backend-dist" ]; then
    rm -rf "$root/backend/dist"
    mv "$rb/backend-dist" "$root/backend/dist"
  fi
}

# 主站回滚：按后端类型恢复上一版并重启（含数据库迁移时的库回滚）
main_rollback() {
  if [ "$MAIN_TYPE" = "python" ]; then
    log "[..] 主站回滚 Python 后端：重置源码树 + 重装依赖 + 重套定制层"
    backend_rollback "$ROOT" python "$ROLLBACK_DIR" "$OLD_HEAD"
    # python 的 schema 演进是 create_all/ensure_schema_drift（只加列，旧代码可读），
    # 不做自动库回滚以免丢失备份后写入的合法数据；本次更新前的库快照保留供人工恢复。
    if [ -n "${DB_BACKUP_DIR:-}" ] && [ -d "$DB_BACKUP_DIR" ]; then
      log "[..] 本次更新前的数据库快照保留在 $DB_BACKUP_DIR（如需回滚请人工恢复）"
    fi
    # 回滚后按当前树重新识别后端类型，再写正确的启动脚本（python→node 切换也成立）
    local rb_type
    rb_type=$(backend_type "$ROOT")
    write_start_sh "$ROOT" "$rb_type" "$LOG_DIR/backend.log"
    pm2 restart samryetha-backend samryetha-frontend >> "$LOG_FILE" 2>&1 || true
  else
    restore_backend; restore_frontend
    if [ -n "${DB_BACKUP_DIR:-}" ]; then
      log "[..] 本次含数据库迁移，一并进行数据库回滚"
      restore_db
    fi
    pm2 restart samryetha-backend samryetha-frontend >> "$LOG_FILE" 2>&1 || true
  fi
  notify error "主站更新失败，已回滚到上一版（详见 update.log）"
}

# ===================== 自适应前端（Node SSR / 静态构建 / 后端内置） =====================
# 与后端同理，按 frontend/ 结构识别前端类型并分派构建/部署/启动：
#   vite-ssr    : React SSR（server.mjs + vite）—— 当前形态，pm2 起独立前端进程
#   vite-static : 纯静态构建（vite，无 server.mjs）—— 由 Caddy 直接托管静态 + /api 反代
#   none        : 后端承担前端（如 FastAPI 单服务）—— 无独立前端，Caddy 反代到后端端口
#   unknown     : frontend/ 存在但无法识别 —— 中止更新，保持旧版运行（安全失败）
frontend_type() {
  if [ -f "$1/frontend/server.mjs" ]; then
    echo "vite-ssr"
  elif [ -f "$1/frontend/vite.config.ts" ] || [ -f "$1/frontend/vite.config.js" ]; then
    echo "vite-static"
  elif [ -d "$1/frontend" ]; then
    echo "unknown"
  else
    echo "none"
  fi
}

frontend_deps() {
  [ -d "$1/frontend" ] || return 0
  (cd "$1/frontend" && pnpm install --ignore-scripts) >> "$LOG_FILE" 2>&1
}

# 构建前端本地 workspace 依赖（frontend/package.json 中 file: 引用的包，如 samryetha-ui-commons / @lako/ui）。
# vite 按 file: 路径解析其构建产物（dist/），而 dist/ 全局被 .gitignore 忽略、git 不跟踪，
# 全新检出或 git reset --hard 后会缺失，导致 vite "failed to resolve import"。
# 用 node 扫描依赖表逐个构建，未来新增本地包也无需改本脚本。
# 注意：pnpm 把 file: 依赖以硬链接形式注入虚拟 store（node_modules/.pnpm），只构建源码树的
# dist 不会反映到解析路径上，必须再 pnpm install 一次让 pnpm 重新注入含 dist 的副本。
frontend_workspace_deps() {
  local root="$1" fe_pkg="$1/frontend/package.json"
  [ -f "$fe_pkg" ] || return 0
  local rc=0 built=0 pkg_dir
  while IFS='|' read -r _ pkg_dir; do
    [ -n "$pkg_dir" ] || continue
    [ -f "$pkg_dir/package.json" ] || { log "[..] workspace 包缺 package.json，跳过: $pkg_dir"; continue; }
    if ! grep -q '"build"' "$pkg_dir/package.json"; then
      log "[..] workspace 包无 build 脚本，跳过: $pkg_dir"
      continue
    fi
    log "==> 构建 workspace 依赖包: $pkg_dir"
    # pnpm 11 默认对未审批的原生依赖（如 sharp）报 ERR_PNPM_IGNORED_BUILDS，
    # 先 --ignore-scripts 装依赖（本应用不依赖原生构建产物，均走预编译二进制），再单独跑 build
    if pnpm --dir "$pkg_dir" install --ignore-scripts >> "$LOG_FILE" 2>&1 \
       && pnpm --dir "$pkg_dir" run build >> "$LOG_FILE" 2>&1; then
      log "[..] workspace 包构建完成: $pkg_dir"
      built=1
    else
      log "[fail] workspace 包构建失败: $pkg_dir"
      rc=1
    fi
  done < <(node - "$fe_pkg" <<'NODE'
const path = require("path");
const fs = require("fs");
const pkg = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const feDir = path.dirname(process.argv[2]);
const all = { ...(pkg.dependencies || {}), ...(pkg.devDependencies || {}) };
for (const [name, spec] of Object.entries(all)) {
  if (typeof spec === "string" && spec.startsWith("file:")) console.log(name + "|" + path.resolve(feDir, spec.slice(5)));
}
NODE
)
  # 重新注入本地包副本（含刚生成的 dist），否则 vite 仍解析到旧 store（无 dist）
  if [ "$rc" = 0 ] && [ "$built" = 1 ]; then
    if ! (cd "$root/frontend" && pnpm install --ignore-scripts) >> "$LOG_FILE" 2>&1; then
      log "[fail] 前端依赖重新注入失败（workspace 包 dist 未同步到 node_modules）"
      return 1
    fi
    log "[..] 前端依赖已重新注入（含 workspace 包构建产物）"
  fi
  return $rc
}

# 前端编译到 stage（vite-ssr: client+ssr；vite-static: client；none: 跳过）
frontend_build() {
  local root="$1" type="$2" stage="$3"
  case "$type" in
    vite-ssr|vite-static)
      frontend_workspace_deps "$root" || return 1
      ;;
  esac
  case "$type" in
    vite-ssr)
      (cd "$root/frontend" && ./node_modules/.bin/vite build --outDir "$stage/client" --emptyOutDir) >> "$LOG_FILE" 2>&1 \
        && (cd "$root/frontend" && ./node_modules/.bin/vite build --ssr src/entry-server.tsx --outDir "$stage/server" --emptyOutDir) >> "$LOG_FILE" 2>&1
      ;;
    vite-static)
      (cd "$root/frontend" && ./node_modules/.bin/vite build --outDir "$stage/client" --emptyOutDir) >> "$LOG_FILE" 2>&1
      ;;
    none)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

frontend_artifact_ok() {
  local type="$2" stage="$3"
  case "$type" in
    vite-ssr)
      [ -f "$stage/client/index.html" ] && [ -f "$stage/server/entry-server.js" ]
      ;;
    vite-static)
      [ -f "$stage/client/index.html" ]
      ;;
    none)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

# 前端产物部署（none 跳过）
frontend_deploy() {
  local root="$1" type="$2" stage="$3" dist="$4"
  case "$type" in
    vite-ssr)
      rsync -a --delete "$stage/" "$dist/"
      ;;
    vite-static)
      rm -rf "$dist/server" 2>/dev/null || true
      rsync -a --delete "$stage/client/" "$dist/client/"
      ;;
    none)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

# 生成站点 Caddy 块（按前端类型：SSR 反代前端端口 / 静态托管 + API 反代 / 反代后端）
site_block() {
  local domain="$1" fe_port="$2" be_port="$3" fe_type="$4" static_root="$5"
  local security=""
  case "$domain" in
    samryetha.com|development.samryetha.com)
      security=$'\timport security_headers\n\timport forum_document_security' ;;
  esac
  case "$fe_type" in
    vite-static)
      cat <<EOF
$domain {
$security
	encode gzip
	handle /api/* {
		reverse_proxy 127.0.0.1:$be_port
	}
	handle {
		root * $static_root
		file_server
	}
}
EOF
      ;;
    none)
      cat <<EOF
$domain {
$security
	reverse_proxy 127.0.0.1:$be_port
}
EOF
      ;;
    *)
      cat <<EOF
$domain {
$security
	reverse_proxy 127.0.0.1:$fe_port
}
EOF
      ;;
  esac
}

# 在 /etc/caddy/Caddyfile 中幂等替换/新增指定域名的站点块（按花括号深度识别块边界）
caddy_replace_block() {
  local domain="$1" block="$2"
  if sudo -n python3 - "$domain" "$block" <<'PYEOF'
import sys
domain, block = sys.argv[1], sys.argv[2]
path = "/etc/caddy/Caddyfile"
with open(path, encoding="utf-8") as f:
    lines = f.read().split("\n")
start = -1
for idx, ln in enumerate(lines):
    s = ln.strip()
    if s == domain or s.rstrip("{").strip() == domain:
        start = idx
        break
if start >= 0:
    depth, j = 0, start
    while j < len(lines):
        depth += lines[j].count("{") - lines[j].count("}")
        j += 1
        if depth <= 0:
            break
    new_lines = lines[:start] + block.rstrip("\n").split("\n") + lines[j:]
else:
    new_lines = lines + [""] + block.rstrip("\n").split("\n")
with open(path, "w", encoding="utf-8") as f:
    f.write("\n".join(new_lines) + "\n")
PYEOF
  then
    return 0
  else
    return 1
  fi
}

# 应用主站 Caddy（仅当前端为静态/无前端时才改写 samryetha.com 块；vite-ssr 保持现状不动）
configure_main_caddy() {
  if sudo -n grep -q "samryetha.com" /etc/caddy/Caddyfile 2>/dev/null && [ "$MAIN_FE_TYPE" = "vite-ssr" ]; then
    return 0
  fi
  log "==> 配置 Caddy（samryetha.com，前端类型 $MAIN_FE_TYPE）"
  local block
  block=$(site_block "samryetha.com" 3000 3001 "$MAIN_FE_TYPE" "$ROOT/frontend/dist/client")
  if ! caddy_replace_block "samryetha.com" "$block"; then
    log "[fail] Caddyfile 改写失败"
    return 1
  fi
  if ! sudo -n caddy validate --config /etc/caddy/Caddyfile >> "$LOG_FILE" 2>&1; then
    log "[fail] Caddyfile 校验失败，请人工检查"
    return 1
  fi
  sudo -n systemctl reload caddy >> "$LOG_FILE" 2>&1 || return 1
  log "[..] Caddy 已更新 samryetha.com"
}

# 保留最近的备份份数，清理更旧的（patch / db / untracked 备份）
prune_backups() {
  local pattern="$1" keep="${2:-10}"
  ls -1dt $LOG_DIR/$pattern 2>/dev/null | tail -n +$((keep + 1)) | while read -r old; do rm -rf "$old"; done
}

# ===================== 主站（main）回滚辅助 =====================
# 回滚目录按被替换的旧 SHA 分目录保存，保留最近 3 版（可回退多个版本）
ROLLBACK_ROOT="$LOG_DIR/rollback"
ROLLBACK_DIR="$ROLLBACK_ROOT/current"
DB_BACKUP_DIR=""
restore_backend() {
  if [ -d "$ROLLBACK_DIR/backend-dist" ]; then
    rm -rf "$BACKEND_DIST"
    mv "$ROLLBACK_DIR/backend-dist" "$BACKEND_DIST"
  fi
}
restore_frontend() {
  if [ -d "$ROLLBACK_DIR/frontend-dist" ]; then
    rm -rf "$FRONTEND_DIST"
    mv "$ROLLBACK_DIR/frontend-dist" "$FRONTEND_DIST"
  fi
}
restore_db() {
  if [ -n "$DB_BACKUP_DIR" ] && [ -d "$DB_BACKUP_DIR" ]; then
    pm2 stop samryetha-backend >> "$LOG_FILE" 2>&1 || true
    # 先清掉当前 WAL/SHM，避免与恢复的 app.db 不一致
    rm -f "$DB_DIR/app.db-wal" "$DB_DIR/app.db-shm"
    for f in $DB_FILES; do
      [ -f "$DB_BACKUP_DIR/$f" ] && cp -f "$DB_BACKUP_DIR/$f" "$DB_DIR/$f"
    done
    log "[..] 数据库已恢复到迁移前备份: $DB_BACKUP_DIR"
  fi
}

# ===================== dev 环境回滚辅助 =====================
dev_restore_dists() {
  if [ -d "$DEV_ROLLBACK_DIR/backend-dist" ]; then
    rm -rf "$DEV_ROOT/backend/dist"
    mv "$DEV_ROLLBACK_DIR/backend-dist" "$DEV_ROOT/backend/dist"
  fi
  if [ -d "$DEV_ROLLBACK_DIR/frontend-dist" ]; then
    rm -rf "$DEV_ROOT/frontend/dist"
    mv "$DEV_ROLLBACK_DIR/frontend-dist" "$DEV_ROOT/frontend/dist"
  fi
}

# dev 回滚：按后端类型恢复上一版（node 恢复旧 dist；python 重置源码树 + 重装依赖 + 重套定制层）
dev_rollback() {
  if [ "$DEV_TYPE" = "python" ]; then
    log "[dev][..] 回滚 Python 后端：重置源码树 + 重装依赖 + 重套定制层"
    backend_rollback "$DEV_ROOT" python "$DEV_ROLLBACK_DIR" "$DEV_OLD_HEAD"
  else
    dev_restore_dists
  fi
  notify error "dev 更新失败，已回滚（详见 update.log）"
}

cd "$ROOT" || { log "[fail] 无法进入 $ROOT"; exit 1; }

# 通用开关：--force 忽略"已是最新"，强制重部署远端最新
FORCE=0
if [ "${1:-}" = "--force" ]; then
  FORCE=1
  shift || true
fi

if [ "${1:-}" = "--status" ]; then
  echo "=== Samryetha 更新状态 ==="
  REMOTE=$(git ls-remote "$REPO_URL" "refs/heads/$BRANCH" 2>/dev/null | awk '{print $1}')
  LAST=""; [ -f "$MARKER" ] && LAST=$(cat "$MARKER")
  HEAD=$(git rev-parse HEAD 2>/dev/null)
  echo "远端最新:   ${REMOTE:-未知}"
  echo "已部署:     ${LAST:-无记录}"
  echo "工作区 HEAD: $HEAD"
  if [ -n "$REMOTE" ] && [ "$REMOTE" != "$LAST" ]; then echo "=> 有可用更新"; else echo "=> 已是最新"; fi
  echo
  echo "=== dev 环境状态（https://$DEV_DOMAIN） ==="
  DEV_REMOTE=$(git ls-remote "$REPO_URL" "refs/heads/$DEV_BRANCH" 2>/dev/null | awk '{print $1}')
  DEV_LAST=""; [ -f "$DEV_MARKER" ] && DEV_LAST=$(cat "$DEV_MARKER")
  DEV_HEAD=""
  [ -d "$DEV_ROOT/.git" ] && DEV_HEAD=$(git -C "$DEV_ROOT" rev-parse HEAD 2>/dev/null)
  echo "dev 远端最新:    ${DEV_REMOTE:-未知}"
  echo "dev 已部署:      ${DEV_LAST:-无记录}"
  echo "dev 工作区 HEAD: ${DEV_HEAD:-未部署（首次运行时全新部署）}"
  if [ -n "$DEV_REMOTE" ] && [ "$DEV_REMOTE" != "$DEV_LAST" ]; then echo "=> dev 有可用更新"; else echo "=> dev 已是最新"; fi
  echo "--- 定制层检查 ---"
  python3 "$CZ" --check
  exit $?
fi

# ---- 手动回滚：update.sh --rollback <sha>（status 后台调用）----
if [ "${1:-}" = "--rollback" ]; then
  TARGET="${2:-}"
  [ -n "$TARGET" ] || { echo "usage: update.sh --rollback <sha>"; exit 2; }
  RB="$ROLLBACK_ROOT/$TARGET"
  if [ ! -d "$RB" ]; then
    log "[fail] 没有该版本的可回滚产物: $RB"
    exit 2
  fi
  cd "$ROOT" || exit 1
  log "==> 手动回滚到 $TARGET"
  MAIN_TYPE=$(backend_type "$ROOT")
  if [ "$MAIN_TYPE" = "python" ]; then
    # 浅克隆可能没有该提交对象，先尝试补齐
    git fetch origin "$TARGET" --depth=1 >> "$LOG_FILE" 2>&1 || true
    backend_rollback "$ROOT" python "$RB" "$TARGET"
  else
    ROLLBACK_DIR="$RB"
    restore_backend; restore_frontend
  fi
  write_start_sh "$ROOT" "$(backend_type "$ROOT")" "$LOG_DIR/backend.log"
  pm2 restart samryetha-backend samryetha-frontend >> "$LOG_FILE" 2>&1 || true
  # 标记指向远端最新，避免下一轮 cron 立刻把回滚版本又更新回去（向前更新不受影响）
  REMOTE=$(git ls-remote "$REPO_URL" "refs/heads/$BRANCH" 2>/dev/null | awk '{print $1}')
  echo "${REMOTE:-$TARGET}" > "$MARKER"
  notify error "主站已手动回滚到 ${TARGET:0:10}"
  log "[ok] 回滚完成：HEAD=$(git rev-parse --short HEAD)，标记=${REMOTE:-$TARGET}"
  exit 0
fi

# ===================== 主站更新（main） =====================
update_main() {
  cd "$ROOT" || return 1
  write_start_sh "$ROOT" "$(backend_type "$ROOT")" "$LOG_DIR/backend.log"
  log "==> 检查更新"

  REMOTE=$(git ls-remote "$REPO_URL" "refs/heads/$BRANCH" 2>>"$LOG_FILE" | awk '{print $1}')
  if [ -z "$REMOTE" ]; then
    log "[fail] 无法从 $REPO_URL 获取 $BRANCH（网络或认证问题），跳过本次更新"
    return 1
  fi

  LAST=""
  [ -f "$MARKER" ] && LAST=$(cat "$MARKER")

  if [ "$REMOTE" = "$LAST" ] && [ "$FORCE" != "1" ]; then
    log "[ok] 已是最新 ($REMOTE)"
    write_config_notice "$ROOT"
    return 0
  fi

  OLD_HEAD=$(git rev-parse HEAD)
  log "==> 发现新版本 $REMOTE（当前 $OLD_HEAD），开始更新"

  if ! git fetch origin "$BRANCH" --depth=1 >> "$LOG_FILE" 2>&1; then
    log "[fail] 拉取上游失败，未重启服务"
    return 1
  fi

  # ---- 冲突预检：上游新增/修改的文件 vs 本地受保护的未跟踪文件 ----
  # git reset --hard 会静默覆盖与目标树同路径的未跟踪文件，必须先备份或中止
  CHANGED_FILES=$(git diff --name-only "$OLD_HEAD" "$REMOTE" 2>/dev/null)
  # 浅克隆下旧提交对象可能已被回收，diff 不可靠 → 保守地强制重装依赖
  FORCE_DEPS=0
  if ! git cat-file -e "$OLD_HEAD" 2>/dev/null; then
    CHANGED_FILES=""
    FORCE_DEPS=1
    log "[..] 旧提交对象不可用（浅克隆），本次强制重装依赖"
  fi
  TS=$(date +%Y%m%d-%H%M%S)
  CRITICAL_HIT=0
  for f in .deploy-credentials update.sh customizations/apply.py customizations/files/smtp.ts opencode/ai-key.txt; do
    if [ -f "$ROOT/$f" ] && echo "$CHANGED_FILES" | grep -qx "$f"; then
      log "[fail] 上游改动与本地受保护文件冲突: $f（为防数据丢失中止更新，请人工处理）"
      CRITICAL_HIT=1
    fi
  done
  [ "$CRITICAL_HIT" = 1 ] && return 1

  for f in backend/src/infrastructure/email/smtp.ts ecosystem.config.cjs; do
    if [ -f "$ROOT/$f" ] && echo "$CHANGED_FILES" | grep -qx "$f"; then
      mkdir -p "$LOG_DIR/untracked-backup-$TS"
      cp -a "$ROOT/$f" "$LOG_DIR/untracked-backup-$TS/$(basename "$f")"
      log "[..] 上游将接管 $f，本地版本已备份至 untracked-backup-$TS/"
    fi
  done
  prune_backups "untracked-backup-*" 10

  # ---- 备份未知本地改动（安全网：任何人工修改绝不丢失）----
  if ! git diff HEAD --quiet 2>/dev/null; then
    git diff HEAD > "$LOG_DIR/local-mods-$TS.patch" 2>>"$LOG_FILE"
    log "[..] 本地改动已备份: local-mods-$TS.patch"
  fi
  prune_backups "local-mods-*.patch" 10

  env_guard_snapshot "$ROOT"

  # ---- 重置到上游，再重新应用定制层 ----
  if ! git reset --hard "$REMOTE" >> "$LOG_FILE" 2>&1; then
    log "[fail] 重置到上游失败"
    return 1
  fi
  log "[..] 已重置到上游 $REMOTE"

  log "==> 应用定制层"
  if ! python3 "$CZ" >> "$LOG_FILE" 2>&1; then
    log "[fail] 定制层应用失败（上游改动需人工适配），服务保持旧版运行"
    log "===== 定制层失败详情（末 20 行）====="
    tail -20 "$LOG_FILE" | grep "^\[cz\]" | while IFS= read -r line; do log "$line"; done
    return 1
  fi
  log "[..] 定制层就绪"

  # ---- 识别后端/前端类型（以重置后的树为准），并施加绑定安全网 ----
  MAIN_TYPE=$(backend_type "$ROOT")
  backend_bind_fix "$ROOT" "$MAIN_TYPE"
  MAIN_FE_TYPE=$(frontend_type "$ROOT")
  ensure_env_from_example "$ROOT"
  if ! env_guard_check "$ROOT"; then
    log "[fail] 后端 .env 校验未通过，中止更新"
    return 1
  fi
  log "[..] 后端类型: $MAIN_TYPE | 前端类型: $MAIN_FE_TYPE"
  if [ "$MAIN_FE_TYPE" = "unknown" ]; then
    log "[fail] 前端类型无法识别（frontend/ 结构异常），中止更新，服务保持旧版运行"
    return 1
  fi

  # ---- 依赖：按后端/前端类型判断是否需安装 ----
  if [ "${FORCE_DEPS:-0}" = "1" ] \
     || backend_deps_needed "$ROOT" "$MAIN_TYPE" "$CHANGED_FILES" \
     || { [ "$MAIN_FE_TYPE" != "none" ] && [ ! -x "$ROOT/frontend/node_modules/.bin/vite" ]; }; then
    log "==> 安装依赖"
    backend_deps "$ROOT" "$MAIN_TYPE" || { log "[fail] 后端依赖安装失败，服务保持旧版运行"; return 1; }
    frontend_deps "$ROOT" || { log "[fail] 前端依赖安装失败，服务保持旧版运行"; return 1; }
  else
    log "[..] 依赖无变化，跳过安装"
  fi

  # ---- 编译到临时目录（绝不触碰运行中的 dist）----
  TMP_BUILD="$(mktemp -d /opt/Samryetha/.build-XXXXXX)"
  STAGE_BACKEND="$TMP_BUILD/stage-backend"
  STAGE_FRONTEND="$TMP_BUILD/stage-frontend"
  mkdir -p "$STAGE_BACKEND" "$STAGE_FRONTEND"

  log "==> 编译（临时目录 $TMP_BUILD）"

  if ! backend_build "$ROOT" "$MAIN_TYPE" "$STAGE_BACKEND"; then
    log "[fail] 后端编译失败，服务保持旧版运行"
    return 1
  fi
  if ! backend_artifact_ok "$ROOT" "$MAIN_TYPE" "$STAGE_BACKEND"; then
    log "[fail] 后端构建产物结构异常，服务保持旧版运行"
    return 1
  fi

  if ! frontend_build "$ROOT" "$MAIN_FE_TYPE" "$STAGE_FRONTEND"; then
    log "[fail] 前端编译失败，服务保持旧版运行"
    return 1
  fi
  if ! frontend_artifact_ok "$ROOT" "$MAIN_FE_TYPE" "$STAGE_FRONTEND"; then
    log "[fail] 前端构建产物结构异常，服务保持旧版运行"
    return 1
  fi

  # ---- 部署 ----
  log "==> 部署构建产物"
  ROLLBACK_DIR="$ROLLBACK_ROOT/$OLD_HEAD"
  rm -rf "$ROLLBACK_DIR"
  mkdir -p "$ROLLBACK_DIR"
  if [ "$MAIN_TYPE" = "node" ]; then
    [ -d "$BACKEND_DIST" ] && cp -a "$BACKEND_DIST" "$ROLLBACK_DIR/backend-dist"
  fi
  [ -d "$FRONTEND_DIST" ] && cp -a "$FRONTEND_DIST" "$ROLLBACK_DIR/frontend-dist"
  prune_backups "rollback/*" 3

  if [ "$MAIN_TYPE" = "node" ]; then
    if ! backend_deploy "$ROOT" "$MAIN_TYPE" "$STAGE_BACKEND"; then
      log "[fail] 部署后端产物失败"
      backend_rollback "$ROOT" "$MAIN_TYPE" "$ROLLBACK_DIR" "$OLD_HEAD"
      return 1
    fi
  fi
  if ! frontend_deploy "$ROOT" "$MAIN_FE_TYPE" "$STAGE_FRONTEND" "$FRONTEND_DIST"; then
    log "[fail] 部署前端产物失败"
    backend_rollback "$ROOT" "$MAIN_TYPE" "$ROLLBACK_DIR" "$OLD_HEAD"
    return 1
  fi

  # ---- 数据库迁移处理 ----
  # node 走 drizzle 迁移文件；python 启动时 create_all/ensure_schema_drift 会改库（只加列）。
  # 两种情况都在更新前留一份一致的在线快照（python 一律留），供人工恢复。
  MIGRATIONS_CHANGED=0
  DB_SNAPSHOT=0
  if [ "$MAIN_TYPE" = "node" ]; then
    MIGRATIONS_CHANGED=$(echo "$CHANGED_FILES" | grep -c "^backend/drizzle/") || true
    [ "${MIGRATIONS_CHANGED:-0}" != "0" ] && DB_SNAPSHOT=1
  else
    DB_SNAPSHOT=1
  fi
  log "==> 滚动重启（前端 cluster 逐个切换，后端优雅 reload）"

  write_start_sh "$ROOT" "$MAIN_TYPE" "$LOG_DIR/backend.log"

  if [ "$MAIN_FE_TYPE" = "vite-ssr" ]; then
    if echo "$CHANGED_FILES" | grep -qx 'frontend/server.mjs'; then
      # PM2 cluster reload reuses the old shared socket. A full recreation is
      # required when the listen address changes from wildcard to loopback.
      pm2 delete samryetha-frontend >> "$LOG_FILE" 2>&1 || true
      frontend_restart='pm2 start ecosystem.config.cjs --only samryetha-frontend'
    else
      frontend_restart='pm2 reload samryetha-frontend'
    fi
    if ! sh -c "$frontend_restart" >> "$LOG_FILE" 2>&1; then
      log "[fail] 前端 reload 失败，回滚产物并保持旧服务"
      backend_rollback "$ROOT" "$MAIN_TYPE" "$ROLLBACK_DIR" "$OLD_HEAD"
      return 1
    fi
  else
    # 非 SSR 前端：不再需要独立前端进程；由 Caddy 直接托管/反代
    pm2 delete samryetha-frontend >> "$LOG_FILE" 2>&1 || true
    configure_main_caddy || log "[..] 主站 Caddy 配置未完成（需人工检查 /etc/caddy/Caddyfile）"
  fi

  if [ "$DB_SNAPSHOT" = "1" ]; then
    log "[..] 更新前备份数据库（一致快照，$MAIN_TYPE）"
    DB_BACKUP_DIR="$LOG_DIR/db-backup-$TS"
    mkdir -p "$DB_BACKUP_DIR"
    if python3 - "$DB_DIR/app.db" "$DB_BACKUP_DIR/app.db" <<'PY' >> "$LOG_FILE" 2>&1
import sqlite3, sys
src = sqlite3.connect(sys.argv[1]); dst = sqlite3.connect(sys.argv[2])
with dst:
    src.backup(dst)
dst.close(); src.close()
PY
    then
      log "[..] 数据库已备份: $DB_BACKUP_DIR/app.db"
    else
      log "[..] 数据库快照失败（继续更新，但本次无库回滚点）"
      DB_BACKUP_DIR=""
    fi
    prune_backups "db-backup-*" 10
  fi

  if [ "${MIGRATIONS_CHANGED:-0}" != "0" ]; then
    # node 迁移：停止后端再启动（迁移在启动时跑），失败连库一起回滚
    pm2 stop samryetha-backend >> "$LOG_FILE" 2>&1 || true
    if ! pm2 restart samryetha-backend >> "$LOG_FILE" 2>&1; then
      log "[fail] 后端启动失败，回滚产物与数据库"
      main_rollback
      return 1
    fi
  else
    if ! pm2 reload samryetha-backend >> "$LOG_FILE" 2>&1; then
      log "[fail] 后端 reload 失败，回滚产物并保持旧服务"
      backend_rollback "$ROOT" "$MAIN_TYPE" "$ROLLBACK_DIR" "$OLD_HEAD"
      return 1
    fi
  fi

  # ---- 健康检查（带重试，容忍优雅切换/迁移所需启动时间）----
  if [ "$MAIN_FE_TYPE" = "vite-ssr" ]; then
    PM2_WANT="samryetha-backend samryetha-frontend"
  else
    PM2_WANT="samryetha-backend"
  fi
  FAILED=0
  for attempt in 1 2 3 4 5 6 7 8 9 10 11 12; do
    PM2_OK=$(PM2_WANT="$PM2_WANT" node -e '
      const { execSync } = require("child_process");
      const apps = JSON.parse(execSync("pm2 jlist").toString());
      const want = process.env.PM2_WANT.split(" ").filter(Boolean);
      const ok = want.every(n => apps.some(a => a.name === n && a.pm2_env.status === "online"));
      process.exit(ok ? 0 : 1);
    ' 2>/dev/null && echo yes || echo no)
    case "$MAIN_FE_TYPE" in
      vite-static)
        FE_OK=$(curl -fsS -o /dev/null -k "https://127.0.0.1/" -H "Host: samryetha.com" 2>/dev/null && echo yes || echo no)
        ;;
      none)
        FE_OK=yes
        ;;
      *)
        FE_OK=$(curl -fsS -o /dev/null "http://127.0.0.1:3000/" 2>/dev/null && echo yes || echo no)
        ;;
    esac
    BE_OK=$(curl -fsS -o /dev/null "http://127.0.0.1:3001/api/health" 2>/dev/null && echo yes || echo no)
    if [ "$PM2_OK" = "yes" ] && [ "$FE_OK" = "yes" ] && [ "$BE_OK" = "yes" ]; then
      log "[..] 健康检查通过（第 ${attempt} 次探测）"
      FAILED=0
      break
    fi
    sleep 3
    FAILED=1
  done

  if [ "$FAILED" = "1" ]; then
    log "[fail] reload 后服务未通过健康检查，回滚到上一版并重启旧服务"
    main_rollback
    return 1
  fi

  echo "$REMOTE" > "$MARKER"
  # 保留最近 3 版回滚目录（不删除），失败时可回退多版
  prune_backups "rollback/*" 3
  log "[ok] 更新完成，已部署 $REMOTE"
  write_config_notice "$ROOT"
  notify success "主站已更新到 ${REMOTE:0:10}"

  # 代码体检：对新部署代码做静态分析（tsc 语义 + ESLint type-checked + 自定义 AST）
  # 新增缺陷自动写入状态页事件；存量问题在基线中不告警
  log "==> 代码体检"
  node "$ROOT/status/codecheck.mjs" >> "$LOG_FILE" 2>&1 || log "[..] 代码体检完成（发现新增问题，已写入事件）"
  return 0
}

# ===================== dev 环境辅助 =====================

# dev 的 .env 从主环境派生：同密钥/同账号（保证同步过来的数据可用），
# 改端口与域名、强制 COOKIE_SECURE，去掉 SMTP 配置（测试环境不向真实用户发邮件，走 ConsoleMailer 仅记日志）
sync_dev_env() {
  if [ ! -f "$ROOT/backend/.env" ]; then
    log "[dev][fail] 主环境 backend/.env 不存在，无法派生 dev 配置"
    return 1
  fi
  {
    # OIDC 的两条回调必须**跟着 dev 域名走**：主环境那两行指向 samryetha.com，
    # 直接继承会让 dev 的 callback 落到主站，而 Lako 侧只注册了 dev 自己的那个
    # redirect_uri，登录会以 invalid_client 直接失败。所以这里先剔除、再按 dev 域写回。
    grep -vE '^(PORT|APP_ORIGIN|COOKIE_SECURE|SMTP_URL|SMTP_FROM|OIDC_REDIRECT_URI|OIDC_POST_LOGOUT_REDIRECT_URI)=' "$ROOT/backend/.env" || true
    cat <<EOF

# --- dev 环境覆盖（update.sh 每次同步数据时自动重写） ---
PORT=$DEV_BE_PORT
APP_ORIGIN=https://$DEV_DOMAIN
COOKIE_SECURE=true
OIDC_REDIRECT_URI=https://$DEV_DOMAIN/api/auth/callback
OIDC_POST_LOGOUT_REDIRECT_URI=https://$DEV_DOMAIN/
EOF
  } > "$DEV_ROOT/backend/.env"
  chmod 600 "$DEV_ROOT/backend/.env"
  log "[dev][..] backend/.env 已从主环境派生（端口 $DEV_BE_PORT，禁用 SMTP 外发）"
}

# 清空 dev 数据并同步主站：删除原 data/uploads，镜像附带目录，
# 主库用 node:sqlite backup() 在线快照（事务一致，主站不停机），最后重派生 .env
sync_dev_data() {
  log "==> [dev] 清空并同步主站数据"
  pm2 stop samryetha-dev-backend >> "$LOG_FILE" 2>&1 || true

  rm -rf "$DEV_ROOT/backend/data" "$DEV_ROOT/backend/uploads"
  mkdir -p "$DEV_ROOT/backend/data" "$DEV_ROOT/backend/uploads"

  # 附带目录（backups、test.db 等）原样镜像，排除运行中的主库文件
  if ! rsync -a --delete --exclude=app.db --exclude=app.db-wal --exclude=app.db-shm \
      "$DB_DIR/" "$DEV_ROOT/backend/data/" >> "$LOG_FILE" 2>&1; then
    log "[dev][fail] dev 数据目录镜像失败"
    return 1
  fi

  if [ ! -f "$DB_DIR/app.db" ]; then
    log "[dev][fail] 主站数据库不存在: $DB_DIR/app.db"
    return 1
  fi
  if ! node -e '
    const { DatabaseSync, backup } = require("node:sqlite");
    const src = new DatabaseSync(process.argv[1], { readOnly: true });
    backup(src, process.argv[2])
      .then(() => { src.close(); process.exit(0); })
      .catch((e) => { console.error(e && e.message || e); process.exit(1); });
  ' "$DB_DIR/app.db" "$DEV_ROOT/backend/data/app.db" >> "$LOG_FILE" 2>&1; then
    log "[dev][fail] 主库快照失败"
    return 1
  fi
  rm -f "$DEV_ROOT/backend/data/app.db-wal" "$DEV_ROOT/backend/data/app.db-shm"
  chmod 700 "$DEV_ROOT/backend/data"
  chmod 600 "$DEV_ROOT/backend/data/app.db"

  if [ -d "$ROOT/backend/uploads" ]; then
    rsync -a --delete "$ROOT/backend/uploads/" "$DEV_ROOT/backend/uploads/" >> "$LOG_FILE" 2>&1
  fi

  sync_dev_env || return 1
  log "[dev][ok] 数据已重置为主站快照（数据库 + 附件）"
}

dev_pm2_start() {
  # 主站同款启动方式：经 start.sh 用 bash exec 直接启动后端。
  # pm2 本身通过 ProcessContainerFork.js 包装启动，ESM 入口的 isMain(import.meta.url ===
  # pathToFileURL(argv[1])) 判断会失败导致 main() 不执行、进程静默 exit 0；start.sh 可让
  # argv[1] 直接指向应用脚本，这是主站采用 start.sh 的原因，dev 保持一致。
  # start.sh 内容按后端类型生成（node 跑 dist，python 跑 uv run）。
  write_start_sh "$DEV_ROOT" "$(backend_type "$DEV_ROOT")" ""

  pm2 delete samryetha-dev-backend samryetha-dev-frontend >> "$LOG_FILE" 2>&1 || true
  if ! NODE_ENV=production pm2 start "$DEV_ROOT/backend/start.sh" \
      --name samryetha-dev-backend --cwd "$DEV_ROOT/backend" \
      --log "$LOG_DIR/dev-backend.log" --time >> "$LOG_FILE" 2>&1; then
    log "[dev][fail] dev 后端启动失败"
    return 1
  fi
  if [ "$(frontend_type "$DEV_ROOT")" = "vite-ssr" ]; then
    if ! NODE_ENV=production PORT=$DEV_FE_PORT API_TARGET="http://127.0.0.1:$DEV_BE_PORT" \
        pm2 start "$DEV_ROOT/frontend/server.mjs" \
        --name samryetha-dev-frontend --cwd "$DEV_ROOT/frontend" \
        --log "$LOG_DIR/dev-frontend.log" --time >> "$LOG_FILE" 2>&1; then
      log "[dev][fail] dev 前端启动失败"
      return 1
    fi
  else
    log "[dev][..] 前端类型非 SSR，无独立 dev 前端进程"
  fi
  pm2 save >> "$LOG_FILE" 2>&1 || true
}

dev_health_check() {
  local attempt PM2_OK FE_OK BE_OK FAILED=1
  local dev_fe_type
  dev_fe_type=$(frontend_type "$DEV_ROOT")
  if [ "$dev_fe_type" = "vite-ssr" ]; then
    PM2_WANT="samryetha-dev-backend samryetha-dev-frontend"
  else
    PM2_WANT="samryetha-dev-backend"
  fi
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    PM2_OK=$(PM2_WANT="$PM2_WANT" node -e '
      const { execSync } = require("child_process");
      const apps = JSON.parse(execSync("pm2 jlist").toString());
      const want = process.env.PM2_WANT.split(" ").filter(Boolean);
      const ok = want.every(n => apps.some(a => a.name === n && a.pm2_env.status === "online"));
      process.exit(ok ? 0 : 1);
    ' 2>/dev/null && echo yes || echo no)
    case "$dev_fe_type" in
      vite-static)
        FE_OK=$(curl -fsS -o /dev/null -k "https://127.0.0.1/" -H "Host: $DEV_DOMAIN" 2>/dev/null && echo yes || echo no)
        ;;
      none)
        FE_OK=yes
        ;;
      *)
        FE_OK=$(curl -fsS -o /dev/null "http://127.0.0.1:$DEV_FE_PORT/" 2>/dev/null && echo yes || echo no)
        ;;
    esac
    BE_OK=$(curl -fsS -o /dev/null "http://127.0.0.1:$DEV_BE_PORT/api/health" 2>/dev/null && echo yes || echo no)
    if [ "$PM2_OK" = "yes" ] && [ "$FE_OK" = "yes" ] && [ "$BE_OK" = "yes" ]; then
      log "[dev][..] 健康检查通过（第 ${attempt} 次探测）"
      FAILED=0
      break
    fi
    sleep 3
    FAILED=1
  done
  return $FAILED
}

# 幂等写入 Caddy 站点块并 reload（按前端类型决定反代/静态托管；证书由 Caddy 自动签发续期）
configure_dev_caddy() {
  local block
  block=$(site_block "$DEV_DOMAIN" "$DEV_FE_PORT" "$DEV_BE_PORT" "$(frontend_type "$DEV_ROOT")" "$DEV_ROOT/frontend/dist/client")
  if ! sudo -n grep -q "$DEV_DOMAIN" /etc/caddy/Caddyfile 2>/dev/null; then
    log "==> [dev] 配置 Caddy（$DEV_DOMAIN）"
    sudo -n tee -a /etc/caddy/Caddyfile >/dev/null <<EOF

# Samryetha dev 环境（dev 分支镜像，数据每次同步自主站）
$block
EOF
  else
    if ! caddy_replace_block "$DEV_DOMAIN" "$block"; then
      log "[dev][fail] Caddyfile 改写失败"
      return 1
    fi
  fi
  if ! sudo -n caddy validate --config /etc/caddy/Caddyfile >> "$LOG_FILE" 2>&1; then
    log "[dev][fail] Caddyfile 校验失败，请人工检查 /etc/caddy/Caddyfile"
    return 1
  fi
  if ! sudo -n systemctl reload caddy >> "$LOG_FILE" 2>&1; then
    log "[dev][fail] caddy reload 失败"
    return 1
  fi
  log "[dev][ok] Caddy 已配置 $DEV_DOMAIN（HTTPS 证书自动签发）"
}

# dev 环境定制层：优先与主站同构；若 dev 分支与定制锚点不兼容，
# 回退为上游原始代码（dev 环境以预览上游为主），仅强制后端只绑定 127.0.0.1（本机未启用 ufw 的安全网）
apply_dev_customizations() {
  DEV_CZ_PKG=0
  if SAMRYETHA_ROOT="$DEV_ROOT" python3 "$CZ" > "$LOG_DIR/cz-dev.out" 2>&1; then
    grep -qx "PKG_CHANGED=1" "$LOG_DIR/cz-dev.out" && DEV_CZ_PKG=1
    log "[dev][..] 定制层已应用"
    return 0
  fi
  log "[dev][..] 定制层不适配 dev 分支，回退为上游原始代码"
  tail -30 "$LOG_DIR/cz-dev.out" | while IFS= read -r line; do log "[dev] $line"; done
  git -C "$DEV_ROOT" reset --hard "$DEV_REMOTE" >> "$LOG_FILE" 2>&1
  # 安全网：强制后端只绑定本机（node 版 sed server.ts；python 版 sed main.py）
  sed -i 's/host: "0\.0\.0\.0"/host: "127.0.0.1"/' "$DEV_ROOT/backend/src/app/server.ts" 2>/dev/null || true
  sed -i 's/host="0\.0\.0\.0"/host="127.0.0.1"/' "$DEV_ROOT/backend/src/samryetha/main.py" 2>/dev/null || true
  return 0
}

# dev 首次部署：克隆 dev 分支 → 依赖 → 定制层 → 编译 → 同步主站数据 → 起服务 → Caddy → 健康检查
dev_bootstrap() {
  log "==> [dev] 未发现 $DEV_ROOT，开始全新部署（$DEV_BRANCH 分支 → https://$DEV_DOMAIN）"
  if [ -e "$DEV_ROOT" ] && [ -n "$(ls -A "$DEV_ROOT" 2>/dev/null)" ]; then
    log "[dev][fail] $DEV_ROOT 已存在且非空，请人工处理后重试"
    return 1
  fi
  # /opt 通常为 root 所有：先用当前用户创建，失败则借助 sudo 创建并移交属主
  if ! mkdir -p "$DEV_ROOT" 2>>"$LOG_FILE"; then
    if ! { sudo -n mkdir -p "$DEV_ROOT" && sudo -n chown "$(id -u):$(id -g)" "$DEV_ROOT"; } >> "$LOG_FILE" 2>&1; then
      log "[dev][fail] 无法创建 $DEV_ROOT（权限不足）"
      return 1
    fi
  fi
  if ! git clone --depth 1 --branch "$DEV_BRANCH" --single-branch "$REPO_URL" "$DEV_ROOT" >> "$LOG_FILE" 2>&1; then
    log "[dev][fail] dev 分支克隆失败"
    return 1
  fi
  DEV_REMOTE=$(git -C "$DEV_ROOT" rev-parse HEAD)
  DEV_TYPE=$(backend_type "$DEV_ROOT")
  DEV_FE_TYPE=$(frontend_type "$DEV_ROOT")
  if [ "$DEV_FE_TYPE" = "unknown" ]; then
    log "[dev][fail] 前端类型无法识别（frontend/ 结构异常）"
    return 1
  fi

  log "==> [dev] 安装依赖"
  # 按后端类型安装（node 用 pnpm；python 用 uv）。
  # dev 分支未内置 pnpm 构建审批，非交互安装时 pnpm 11 会以 ERR_PNPM_IGNORED_BUILDS
  # 直接失败；本应用不依赖原生构建产物（esbuild/argon2 走预编译二进制），故 --ignore-scripts。
  backend_deps "$DEV_ROOT" "$DEV_TYPE" || { log "[dev][fail] dev 后端依赖安装失败"; return 1; }
  frontend_deps "$DEV_ROOT" || { log "[dev][fail] dev 前端依赖安装失败"; return 1; }

  log "==> [dev] 应用定制层"
  apply_dev_customizations
  backend_bind_fix "$DEV_ROOT" "$DEV_TYPE"

  log "==> [dev] 编译"
  if ! backend_build "$DEV_ROOT" "$DEV_TYPE" "$DEV_ROOT/backend/dist"; then
    log "[dev][fail] dev 后端编译/校验失败"
    return 1
  fi
  if ! backend_artifact_ok "$DEV_ROOT" "$DEV_TYPE" "$DEV_ROOT/backend/dist"; then
    log "[dev][fail] dev 后端构建产物结构异常"
    return 1
  fi
  if ! frontend_build "$DEV_ROOT" "$DEV_FE_TYPE" "$DEV_ROOT/frontend/dist"; then
    log "[dev][fail] dev 前端编译失败"
    return 1
  fi
  if ! frontend_artifact_ok "$DEV_ROOT" "$DEV_FE_TYPE" "$DEV_ROOT/frontend/dist"; then
    log "[dev][fail] dev 前端构建产物结构异常"
    return 1
  fi

  sync_dev_data || return 1
  dev_pm2_start || return 1
  configure_dev_caddy || log "[dev][..] Caddy 配置未完成（不影响本机健康检查，需人工处理）"

  if ! dev_health_check; then
    log "[dev][fail] dev 首次部署健康检查未通过，请查看 $LOG_DIR/dev-backend.log 与 $LOG_DIR/dev-frontend.log"
    return 1
  fi

  echo "$DEV_REMOTE" > "$DEV_MARKER"
  log "[dev][ok] 全新部署完成：https://$DEV_DOMAIN（$DEV_REMOTE，数据为主站快照）"
}

# dev 后续更新：拉取上游 → 定制层 → 编译到临时目录 → 原子替换 → 清空并同步主站数据 → 重启 → 健康检查
dev_update() {
  cd "$DEV_ROOT" || { log "[dev][fail] 无法进入 $DEV_ROOT"; return 1; }

  DEV_REMOTE=$(git ls-remote "$REPO_URL" "refs/heads/$DEV_BRANCH" 2>>"$LOG_FILE" | awk '{print $1}')
  if [ -z "$DEV_REMOTE" ]; then
    log "[dev][fail] 无法获取 $DEV_BRANCH 分支最新提交（网络或认证问题），跳过 dev 更新"
    return 1
  fi
  DEV_LAST=""
  [ -f "$DEV_MARKER" ] && DEV_LAST=$(cat "$DEV_MARKER")
  if [ "$DEV_REMOTE" = "$DEV_LAST" ] && [ "$FORCE" != "1" ]; then
    log "[dev][ok] 已是最新 ($DEV_REMOTE)"
    return 0
  fi

  DEV_OLD_HEAD=$(git rev-parse HEAD)
  log "==> [dev] 发现新版本 $DEV_REMOTE（当前 $DEV_OLD_HEAD），开始更新"

  if ! git fetch origin "$DEV_BRANCH" --depth=1 >> "$LOG_FILE" 2>&1; then
    log "[dev][fail] dev 分支拉取上游失败，未重启服务"
    return 1
  fi

  DEV_TS=$(date +%Y%m%d-%H%M%S)
  # 安全网：备份 dev 检出内未知的人工改动
  if ! git diff HEAD --quiet 2>/dev/null; then
    git diff HEAD > "$LOG_DIR/dev-local-mods-$DEV_TS.patch" 2>>"$LOG_FILE"
    log "[dev][..] dev 本地改动已备份: dev-local-mods-$DEV_TS.patch"
  fi
  prune_backups "dev-local-mods-*.patch" 10

  DEV_CHANGED_FILES=$(git diff --name-only "$DEV_OLD_HEAD" "$DEV_REMOTE" 2>/dev/null)
  # 旧提交对象可能已被清理（浅克隆），diff 不可靠时保守地强制重装依赖
  if ! git cat-file -e "$DEV_OLD_HEAD" 2>/dev/null; then
    DEV_CHANGED_FILES=""
    DEV_FORCE_INSTALL=1
  else
    DEV_FORCE_INSTALL=0
  fi

  if ! git reset --hard "$DEV_REMOTE" >> "$LOG_FILE" 2>&1; then
    log "[dev][fail] dev 重置到上游失败"
    return 1
  fi
  log "[dev][..] 已重置到上游 $DEV_REMOTE"

  log "==> [dev] 应用定制层"
  apply_dev_customizations
  DEV_TYPE=$(backend_type "$DEV_ROOT")
  DEV_FE_TYPE=$(frontend_type "$DEV_ROOT")
  backend_bind_fix "$DEV_ROOT" "$DEV_TYPE"
  log "[dev][..] 后端类型: $DEV_TYPE | 前端类型: $DEV_FE_TYPE"
  if [ "$DEV_FE_TYPE" = "unknown" ]; then
    log "[dev][fail] 前端类型无法识别（frontend/ 结构异常），服务保持旧版运行"
    return 1
  fi

  if [ "$DEV_FORCE_INSTALL" = "1" ] || [ "$DEV_CZ_PKG" = "1" ] \
     || backend_deps_needed "$DEV_ROOT" "$DEV_TYPE" "$DEV_CHANGED_FILES" \
     || { [ "$DEV_FE_TYPE" != "none" ] && [ ! -x "$DEV_ROOT/frontend/node_modules/.bin/vite" ]; }; then
    log "==> [dev] 安装依赖"
    backend_deps "$DEV_ROOT" "$DEV_TYPE" || { log "[dev][fail] dev 后端依赖安装失败，服务保持旧版运行"; return 1; }
    frontend_deps "$DEV_ROOT" || { log "[dev][fail] dev 前端依赖安装失败，服务保持旧版运行"; return 1; }
  else
    log "[dev][..] 依赖无变化，跳过安装"
  fi

  # 编译到临时目录（绝不触碰运行中的 dist）
  DEV_TMP_BUILD="$(mktemp -d /opt/Samryetha/.devbuild-XXXXXX)"
  DEV_STAGE_BACKEND="$DEV_TMP_BUILD/stage-backend"
  DEV_STAGE_FRONTEND="$DEV_TMP_BUILD/stage-frontend"
  mkdir -p "$DEV_STAGE_BACKEND" "$DEV_STAGE_FRONTEND"

  log "==> [dev] 编译（临时目录 $DEV_TMP_BUILD）"
  if ! backend_build "$DEV_ROOT" "$DEV_TYPE" "$DEV_STAGE_BACKEND"; then
    log "[dev][fail] dev 后端编译失败，服务保持旧版运行"
    return 1
  fi
  if ! backend_artifact_ok "$DEV_ROOT" "$DEV_TYPE" "$DEV_STAGE_BACKEND"; then
    log "[dev][fail] dev 后端构建产物结构异常，服务保持旧版运行"
    return 1
  fi
  if ! frontend_build "$DEV_ROOT" "$DEV_FE_TYPE" "$DEV_STAGE_FRONTEND"; then
    log "[dev][fail] dev 前端编译失败，服务保持旧版运行"
    return 1
  fi
  if ! frontend_artifact_ok "$DEV_ROOT" "$DEV_FE_TYPE" "$DEV_STAGE_FRONTEND"; then
    log "[dev][fail] dev 前端构建产物结构异常，服务保持旧版运行"
    return 1
  fi

  log "==> [dev] 部署构建产物"
  rm -rf "$DEV_ROLLBACK_DIR"
  mkdir -p "$DEV_ROLLBACK_DIR"
  if [ "$DEV_TYPE" = "node" ]; then
    [ -d "$DEV_ROOT/backend/dist" ] && cp -a "$DEV_ROOT/backend/dist" "$DEV_ROLLBACK_DIR/backend-dist"
  fi
  [ -d "$DEV_ROOT/frontend/dist" ] && cp -a "$DEV_ROOT/frontend/dist" "$DEV_ROLLBACK_DIR/frontend-dist"

  if [ "$DEV_TYPE" = "node" ]; then
    if ! backend_deploy "$DEV_ROOT" "$DEV_TYPE" "$DEV_STAGE_BACKEND"; then
      log "[dev][fail] dev 后端产物部署失败"
      dev_rollback
      return 1
    fi
  fi
  if ! frontend_deploy "$DEV_ROOT" "$DEV_FE_TYPE" "$DEV_STAGE_FRONTEND" "$DEV_ROOT/frontend/dist"; then
    log "[dev][fail] dev 前端产物部署失败"
    dev_rollback
    return 1
  fi

  # 数据是主站分身：产物就绪后清空重建（此时 dev 后端已被停掉）
  if ! sync_dev_data; then
    log "[dev][fail] dev 数据同步失败，回滚并尝试恢复服务"
    dev_rollback
    sync_dev_data || true
    dev_pm2_start || true
    return 1
  fi

  if ! dev_pm2_start; then
    log "[dev][fail] dev 服务启动失败，回滚并尝试恢复服务"
    dev_rollback
    sync_dev_data || true
    dev_pm2_start || true
    return 1
  fi

  configure_dev_caddy || log "[dev][..] Caddy 配置未完成（不影响本机健康检查，需人工处理）"

  if ! dev_health_check; then
    log "[dev][fail] dev 更新后健康检查未通过，回滚到上一版并恢复服务"
    dev_rollback
    sync_dev_data || true
    dev_pm2_start || true
    return 1
  fi

  echo "$DEV_REMOTE" > "$DEV_MARKER"
  rm -rf "$DEV_ROLLBACK_DIR"
  log "[dev][ok] dev 更新完成：https://$DEV_DOMAIN（$DEV_REMOTE，数据已重置为主站快照）"
}

update_dev() {
  if [ ! -d "$DEV_ROOT/.git" ]; then
    dev_bootstrap
  else
    dev_update
  fi
}

# ===================== 调度：主站与 dev 相互独立，互不阻塞 =====================
# 自动更新开关由 status 后台 /update 写 status/update-config.json 控制（缺省开启）
if [ "$(config_get main.enabled true)" = "true" ]; then
  update_main
  MAIN_RC=$?
else
  log "[skip] 主站自动更新已在后台暂停"
  MAIN_RC=0
fi

if [ "$(config_get dev.enabled true)" = "true" ]; then
  update_dev
  DEV_RC=$?
else
  log "[skip] dev 自动更新已在后台暂停"
  DEV_RC=0
fi

# ---- ops 自更新（更新站自身：状态页 + 更新控制台）----
# 放在最后：即使主站/dev 更新失败，运维组件也应保持最新可用（它正是恢复手段）。
# 自身失败不影响主站结论，只记录退出码。
OPS_RC=0
if [ "$(config_get ops.enabled true)" = "true" ]; then
  OPS_SCRIPT="$ROOT/ops_self_update.sh"
  if [ -f "$OPS_SCRIPT" ]; then
    if ! bash "$OPS_SCRIPT" >> "$LOG_FILE" 2>&1; then
      log "[fail] ops 自更新失败（不影响主站）；详见 ops-update.log"
      OPS_RC=1
    fi
  fi
else
  log "[skip] ops 自更新已在后台暂停"
fi

if [ "$MAIN_RC" -ne 0 ] || [ "$DEV_RC" -ne 0 ]; then
  exit 1
fi
exit 0
