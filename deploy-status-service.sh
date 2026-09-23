#!/bin/bash
# 部署 samryetha-status 服务：systemd 单元 + Caddy /update 反代。幂等。
#
# 用法：在仓库根目录执行 ./deploy-status-service.sh
#
# 需要（不入库）：
#   /opt/Samryetha/status/.service-secret         会话签名密钥
#   /opt/Samryetha/status/update-service/bin/update-service   二进制
#     （由 CI 产出的 Release 附件提供，见 .github/workflows/build-update-service.yml）
set -euo pipefail

ROOT="${SAMRYETHA_ROOT:-/opt/Samryetha}"
SVC_DIR="$ROOT/status/update-service"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TS=$(date +%Y%m%d-%H%M%S)

if [ ! -x "$SVC_DIR/bin/update-service" ]; then
  echo "[fail] 找不到可执行文件 $SVC_DIR/bin/update-service" >&2
  echo "       先下载 CI 产物：curl -fsSL <release>/update-service -o $SVC_DIR/bin/update-service && chmod +x ..." >&2
  exit 1
fi

# 1) systemd
sudo -n cp "$REPO_DIR/update-service/samryetha-status.service" /etc/systemd/system/samryetha-status.service
sudo -n systemctl daemon-reload
sudo -n systemctl enable samryetha-status >/dev/null 2>&1 || true

# 2) Caddy：替换 status.samryetha.com 块（块内容由 caddy-status.conf 提供，不依赖外部临时文件）
sudo -n cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak-$TS"
sudo -n python3 - "$REPO_DIR/caddy-status.conf" "$ROOT" <<'PY'
import sys
block_path, root = sys.argv[1], sys.argv[2]
block = open(block_path, encoding="utf-8").read().rstrip("\n").replace("/opt/Samryetha", root.rstrip("/")).split("\n")
path = "/etc/caddy/Caddyfile"
lines = open(path, encoding="utf-8").read().split("\n")
start = -1
for i, ln in enumerate(lines):
    s = ln.strip()
    if s == "status.samryetha.com" or s.rstrip("{").strip() == "status.samryetha.com":
        start = i
        break
if start < 0:
    lines = lines + [""] + block
else:
    depth, j = 0, start
    while j < len(lines):
        depth += lines[j].count("{") - lines[j].count("}")
        j += 1
        if depth <= 0:
            break
    lines = lines[:start] + block + lines[j:]
open(path, "w", encoding="utf-8").write("\n".join(lines) + "\n")
print("[caddy] status block replaced")
PY
sudo -n caddy validate --config /etc/caddy/Caddyfile

# 3) 重启服务 + reload caddy
sudo -n systemctl restart samryetha-status
sleep 1
sudo -n systemctl reload caddy

echo "==== service status ===="
sudo -n systemctl is-active samryetha-status || true
echo "==== healthz ===="
curl -sS -o /dev/null -w "local /healthz -> %{http_code}\n" http://127.0.0.1:3030/healthz || true
echo "==== /update（应 302 到 auth.samryetha.com） ===="
curl -sS -o /dev/null -w "%{http_code} -> %{redirect_url}\n" http://127.0.0.1:3030/update || true
echo "==== 公开状态页仍 200 ===="
curl -sS -o /dev/null -w "https://status.samryetha.com/ -> %{http_code}\n" https://status.samryetha.com/ || true
