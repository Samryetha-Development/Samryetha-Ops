#!/usr/bin/env bash
set -euo pipefail
script=$(realpath "$(dirname "$0")/build-forum-frontend.sh")
fixture=$(mktemp -d)
trap 'rm -rf -- "$fixture"' EXIT
mkdir -p "$fixture/bin"
cat > "$fixture/bin/corepack" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
test "$1" = pnpm@10.33.2
shift
test "$PWD" != "$TEST_ROOT/frontend"
test "${FAIL_STEP:-}" != "$1"
case "$1" in
  install)
    test "$2" = --frozen-lockfile
    test "$3" = --store-dir
    test ! -e "$4"
    test ! -e node_modules
    test ! -e dist
    mkdir -p node_modules dist
    printf 'new dependencies' > node_modules/marker
    ;;
  build) printf 'new artifacts' > dist/marker ;;
  check:artifacts) test -f dist/marker ;;
esac
MOCK
cat > "$fixture/bin/node" <<'MOCK'
#!/usr/bin/env bash
if [ "$1" = -e ]; then printf 'pnpm@10.33.2\n'; exit 0; fi
test "${FAIL_STEP:-}" != auth
MOCK
chmod +x "$fixture/bin/corepack" "$fixture/bin/node"
export PATH="$fixture/bin:$PATH"
for fail in install typecheck build check:artifacts auth none; do
  export TEST_ROOT="$fixture/${fail//:/_}" FAIL_STEP="$fail"
  mkdir -p "$TEST_ROOT/frontend"/{scripts,node_modules,dist}
  printf 'lockfile' > "$TEST_ROOT/frontend/pnpm-lock.yaml"
  printf 'checker' > "$TEST_ROOT/frontend/scripts/check-artifacts.mjs"
  printf 'old dependencies' > "$TEST_ROOT/frontend/node_modules/marker"
  printf 'old artifacts' > "$TEST_ROOT/frontend/dist/marker"
  if bash "$script" "$TEST_ROOT"; then
    test "$fail" = none
    test "$(cat "$TEST_ROOT/frontend/dist/marker")" = 'new artifacts'
    test "$(cat "$TEST_ROOT/frontend/node_modules/marker")" = 'new dependencies'
  else
    test "$fail" != none
    test "$(cat "$TEST_ROOT/frontend/dist/marker")" = 'old artifacts'
    test "$(cat "$TEST_ROOT/frontend/node_modules/marker")" = 'old dependencies'
  fi
  test -z "$(find "$TEST_ROOT" -maxdepth 1 -name '.frontend-build.*' -print)"
  printf 'ok deployment gate: %s\n' "$fail"
done
