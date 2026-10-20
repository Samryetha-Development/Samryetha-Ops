#!/usr/bin/env bash
set -euo pipefail
exec > >(tee /opt/nedmori-config/build.log) 2>&1

# Build only committed source. The served release and mutable data are untouched.
root=$(realpath "${1:?usage: build-nedmori.sh /absolute/git/checkout}")
revision=$(git -C "$root" rev-parse HEAD)
release=$(mktemp -d "/opt/nedmori-releases/${revision:0:12}.XXXXXXXX")
git -C "$root" archive HEAD | tar -xf - -C "$release"
cd "$release"
export UV_LINK_MODE=copy
uv sync --frozen --python 3.14 --extra dev
manager=$(node -p 'JSON.parse(require("fs").readFileSync("package.json", "utf8")).packageManager')
[[ "$manager" =~ ^pnpm@[0-9]+\.[0-9]+\.[0-9]+$ ]]
corepack "$manager" install --frozen-lockfile --store-dir "$release/.pnpm-store"
corepack "$manager" check
# The 200-row jsdom fixture takes ~7s on this host. Keep every assertion, use one
# test worker and a bounded 15s case budget; the default 5s is workstation-sized.
corepack "$manager" exec vitest run --maxWorkers=1 --testTimeout=15000
.venv/bin/python -m pytest -q
corepack "$manager" build
corepack "$manager" verify:artifacts

# No demo-auth fallback is allowed on the public deployment.
.venv/bin/python - /opt/nedmori-config/.env <<'PY'
import sys
from pathlib import Path
from urllib.parse import urlsplit
from nedmori.__main__ import load_env
from nedmori.auth.config import auth_config_from_env
load_env(sys.argv[1])
config = auth_config_from_env()
assert config.get('enabled') is True, 'Production requires configured OIDC'
assert urlsplit(config['appOrigin']).scheme == 'https'
assert urlsplit(config['issuer']).scheme == 'https'
assert Path('/opt/nedmori-data').is_dir()
PY
if [ -e backend/data ]; then
  # The suite may create disposable default stores. Never serve them as live data.
  test ! -L backend/data
  mv backend/data .test-data
  chmod 700 .test-data
fi
ln -s /opt/nedmori-data backend/data
ln -s /opt/nedmori-config/.env .env
chmod 755 "$release"
printf '%s\n' "$revision" > RELEASE_REVISION
printf '%s\n' "$release" > "$root/.candidate-release.tmp"
mv "$root/.candidate-release.tmp" "$root/.candidate-release"
printf 'Validated release ready: %s\n' "$release"
