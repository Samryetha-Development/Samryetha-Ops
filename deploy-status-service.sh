#!/bin/bash
# 部署 samryetha-status 服务：systemd 单元 + Caddy /update 反代。幂等。
set -euo pipefail
TS=$(date +%Y%m%d-%H%M%S)
SVC_DIR=/opt/Samryetha/status/update-service

# 1) systemd
sudo -n cp "$SVC_DIR/samryetha-status.service" /etc/systemd/system/samryetha-status.service
sudo -n systemctl daemon-reload
sudo -n systemctl enable samryetha-status >/dev/null 2>&1 || true

# 2) Caddy：替换 status.samryetha.com 块
sudo -n cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak-$TS"
sudo -n python3 - <<'PY'
block = open("/tmp/status-block.txt", encoding="utf-8").read().rstrip("\n").split("\n")
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
echo "==== /update (local, 应 302 到 auth.samryetha.com) ===="
curl -sS -o /dev/null -w "%{http_code} -> %{redirect_url}\n" http://127.0.0.1:3030/update || true
echo "==== 公开状态页仍 200 ===="
curl -sS -o /dev/null -w "https://status.samryetha.com/ -> %{http_code}\n" https://status.samryetha.com/ || true
echo "==== /update 经 Caddy ===="
curl -sS -o /dev/null -w "https://status.samryetha.com/update -> %{http_code} -> %{redirect_url}\n" https://status.samryetha.com/update || true
