# Nedmori first production release · 2026-10-20

Public site: https://nedmori.samryetha.com/home

Application revision: `906cd49a4bcd883eb891f3766b1994f5e011b943` from the private
Nedmori main branch. Ops target `nedmori` returned `succeeded`; its marker and the
running release match that revision. The site uses a fresh persistent data
directory; local development identities, databases and test submissions were
not uploaded. Existing Samryetha remains healthy.

## Executed checks

- Frozen pnpm and Python installs in a new release; TypeScript passed.
- Frontend: 33 files / 216 tests passed on the actual host. Its 200-row jsdom
  fixture takes approximately 7 seconds, so the deployment runner uses one worker
  and a bounded 15-second test budget. Assertions were preserved.
- Python 3.14.7 on Linux: 256 passed, 1 skipped; all nine actual Docker integration
  tests ran, including compilation, timeout, output/memory limits and worker
  orphan recovery. Node compatibility was then explicitly rerun with the
  deployment's Node 24: both parity tests passed.
- Vite production build and artifact verification passed: 70 frontend files,
  68 asset references and 73 vendor files matched the checked-in package.
- Actual live API reports a ready process worker and ready C++ 17, Python 3 and
  Java 17 executors. Public HTTPS pages and their entry assets return 200;
  versioned assets are immutable, HTML uses no-cache, security headers are present.
- Anonymous administration is denied, authentication is enabled, and source/data
  paths do not expose files. OIDC initiation uses the exact new HTTPS callback,
  `nedmori-production`, S256 PKCE and Secure/HttpOnly transaction cookies.
- Browser: homepage, a real published problem, formula rendering, Monaco editor
  loading and the return-home breadcrumb were checked on the public domain.
- Live deployment YAML passed the Ops configuration checker with all four targets.

## Failure handling exercised

The kernel's own update initially interrupted a build; the partial release was
not activated. Its update timer was paused for the release and restored afterward.
The original 5-second jsdom budget also blocked activation, as intended.
Test-created default stores were moved into a private `.test-data` directory
before linking the real data directory. PM2 configuration was corrected to the
supported JSON format and the same Node/PM2 path as the kernel service. Activation
was retried through Ops using the exact already-validated candidate; auth and
artifacts were checked again. No partially tested candidate was promoted.

On-server evidence, private to the operator: `/opt/nedmori-config/` contains
`deployment-result.json`, `public-verification.json`, `node-parity.log` and build
logs; the active release retains `BUILD_VERIFICATION.log`. Stopped-writer data
snapshots are in `/opt/nedmori-backups`. The kernel update timer is active again.

## Verification limits

No real person's login consent/callback or authenticated public submission was
performed. Protocol tests and the real Docker worker/submission integration ran
with disposable identities/data. No existing user was granted administration.
This is one Python API process; distributed/multi-writer operation, capacity SLAs,
power-loss recovery and restoration of future real production backups remain
outside this deployment verification.
