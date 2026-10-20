#!/usr/bin/env bash
set -euo pipefail
export PATH=/home/ubuntu/.nvm/versions/node/v24.20.0/bin:/home/ubuntu/.local/bin:$PATH
root=/opt/Nedmori
release=$(cat "$root/.candidate-release")
[[ "$release" == /opt/nedmori-releases/* && -f "$release/RELEASE_REVISION" ]]
previous=""
if [ -L "$root/current" ]; then
  previous=$(readlink -e "$root/current" || true)
fi
rollback() {
  trap - ERR
  pm2 stop nedmori-api >/dev/null 2>&1 || true
  if [ -n "$previous" ] && [ -d "$previous" ]; then
    ln -s "$previous" "$root/.current-rollback"
    mv -Tf "$root/.current-rollback" "$root/current"
    pm2 startOrRestart /opt/Samryetha/kernel/etc/nedmori.config.json --only nedmori-api --update-env || true
  fi
  printf 'Nedmori activation failed. Backup retained; schema rollback requires operator review.\n' >&2
  exit 1
}
trap rollback ERR
pm2 describe nedmori-api >/dev/null 2>&1 && pm2 stop nedmori-api
mkdir -p /opt/nedmori-backups
chmod 700 /opt/nedmori-backups
tar -czf "/opt/nedmori-backups/$(date -u +%Y%m%dT%H%M%SZ).tgz" -C /opt/nedmori-data .
if [ -n "$previous" ] && [ -d "$previous" ]; then
  printf '%s\n' "$previous" > "$root/.previous-release"
fi
ln -s "$release" "$root/.current-next"
mv -Tf "$root/.current-next" "$root/current"
pm2 startOrRestart /opt/Samryetha/kernel/etc/nedmori.config.json --only nedmori-api --update-env
for attempt in $(seq 1 30); do
  if python3 /opt/Samryetha/kernel/scripts/health-nedmori.py; then
    pm2 save
    exit 0
  fi
  sleep 1
done
rollback
