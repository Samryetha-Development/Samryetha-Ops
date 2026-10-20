#!/usr/bin/env bash
set -euo pipefail

# Build in a disposable workspace/store. Never let cached node_modules or an old
# dist make an incomplete dependency look healthy, and never replace live output
# until both artifact integrity and SSR smoke checks pass.
root=$(realpath "${1:?usage: build-forum-frontend.sh /absolute/forum/root}")
frontend="$root/frontend"
test -f "$frontend/pnpm-lock.yaml"
test -f "$frontend/scripts/check-artifacts.mjs"
packageManager=$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")).packageManager)' "$frontend/package.json")
[[ "$packageManager" =~ ^pnpm@[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  printf 'A pinned pnpm packageManager is required\n' >&2
  exit 1
}
staging=$(mktemp -d "$root/.frontend-build.XXXXXXXX")
cleanup() { rm -rf -- "$staging"; }
trap cleanup EXIT
mkdir -p "$staging/frontend" "$staging/bin"
# Git dependency prepare hooks may invoke pnpm again. Keep those nested installs
# on the same pinned manager and disposable store, too.
printf '#!/usr/bin/env bash\nexec corepack %s "$@"\n' "$packageManager" > "$staging/bin/pnpm"
chmod +x "$staging/bin/pnpm"
tar -C "$frontend" --exclude='./node_modules' --exclude='./dist' --exclude='./.env*' -cf - . |
  tar -C "$staging/frontend" -xf -
(
  cd "$staging/frontend"
  export PATH="$staging/bin:$PATH"
  export npm_config_store_dir="$staging/store"
  pnpm install --frozen-lockfile --store-dir "$staging/store"
  pnpm typecheck
  pnpm build
  # Explicit gate also covers revisions whose build script does not invoke it.
  pnpm check:artifacts
  node scripts/auth-redirect-smoke.mjs
)

backup=$(mktemp -d "$root/.frontend-previous.XXXXXXXX")
if [ -d "$frontend/dist" ]; then mv "$frontend/dist" "$backup/dist"; fi
if [ -d "$frontend/node_modules" ] && ! mv "$frontend/node_modules" "$backup/node_modules"; then
  [ ! -d "$backup/dist" ] || mv "$backup/dist" "$frontend/dist"
  exit 1
fi
if ! mv "$staging/frontend/node_modules" "$frontend/node_modules" ||
   ! mv "$staging/frontend/dist" "$frontend/dist"; then
  # Preserve failed promotion output separately, then restore the previous pair.
  [ ! -d "$frontend/node_modules" ] || mv "$frontend/node_modules" "$staging/failed-node_modules"
  [ ! -d "$frontend/dist" ] || mv "$frontend/dist" "$staging/failed-dist"
  [ ! -d "$backup/node_modules" ] || mv "$backup/node_modules" "$frontend/node_modules"
  [ ! -d "$backup/dist" ] || mv "$backup/dist" "$frontend/dist"
  exit 1
fi
printf 'Frontend integrity gate passed; previous artifacts retained at %s\n' "$backup"
