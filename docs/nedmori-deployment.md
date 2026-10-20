# Nedmori production target

Public origin: `https://nedmori.samryetha.com`. The existing Samryetha Ops kernel
manages a separate `nedmori` target from the private Nedmori repository. It uses
the checkout's read-only GitHub deploy key; no personal GitHub token is copied.

## Layout and acceptance

- `/opt/Nedmori`: Git checkout, candidate/previous release pointers and `current` symlink.
- `/opt/nedmori-releases`: immutable source, isolated Python 3.14 environment,
  pinned pnpm install and built frontend per release.
- `/opt/nedmori-data`: private persistent identity/catalog/organization/contest
  databases, submissions, redo journal and outbox. Releases link `backend/data`
  here. Never copy development databases into this directory.
- `/opt/nedmori-config/.env`: private OIDC and deployment configuration, excluded
  from Git. Public PKCE client `nedmori-production`, exact HTTPS callback,
  explicit provider consent. Authentication must be enabled before activation.
- `/opt/nedmori-backups`: stopped-writer snapshots; includes the entire data
  directory, including SQLite WAL files and submission journals. Keep private.
- PM2 `nedmori-api`: **one** Python writer on loopback `8791`, with its process
  worker and Docker executor. Never enable PM2 clustering or Uvicorn workers.
- Caddy serves only `current/dist`, proxies `/api/*`, handles SPA refreshes,
  caches hashed assets and enforces the existing TLS/security headers.

Append `etc/nedmori-target.yaml` to the live `targets` list and install the three
scripts and PM2 configuration into the kernel's corresponding directories.
The target has no automatic schedule by default. Run it through the authenticated
Ops `deployer.deploy` action with `target: nedmori`; unrelated targets are preserved.
The Git driver fetches each checkout's own `origin`.
For the initial long install, stop `samryetha-kernel-update.timer` temporarily
and start it again after verification. An orchestrator self-update/restart cancels
its in-flight build tasks; an interrupted build must never be treated as deployed.

Build runs frozen installs, TypeScript, Vitest, Python tests, Vite and frontend
artifact verification in a fresh release. Python Docker integration tests must
run on the actual deployment host; an unavailable Docker engine is not evidence
of successful isolation. The judge image must be built with the application's
`python -m nedmori.judge.build` source label. No production data is used in tests.

Activation stops the previous writer, saves a full data snapshot, switches the
release symlink, starts PM2 and checks actual Docker readiness and enabled auth.
Failed builds never change the current pointer. Failed activation attempts restore
the previous pointer/process when available, retaining the failed release and
data snapshot for inspection. A first-deployment failure has no previous release.
Inspect both the Ops outcome and `/opt/nedmori-config/deployment-result.json`.

## Rollback and operation

Do not assume the generic Git reset changes the served release: `current` is the
actual runtime. Stop `nedmori-api`, snapshot all data, inspect schema compatibility,
then point `current` to the recorded `.previous-release` and start the PM2 app.
Run `scripts/health-nedmori.py`, check the public HTTPS domain, login redirect and
a problem page. Database restoration is a separate stopped-writer operation;
never restore only submissions JSON or delete an unacknowledged journal.
Retain paired data snapshots and release identifiers. Avoid updates while judges
are busy; interrupted jobs follow the application's explicit recovery behavior.

New OIDC users remain members. Administration uses the existing local-role CLI
after verifying the intended immutable account identity; no first-user promotion.
Real-user password entry, provider consent and the complete account callback
require a user-driven login. Do not claim SMTP/provider/account flows were tested
merely because discovery, redirects or application tests succeeded.
