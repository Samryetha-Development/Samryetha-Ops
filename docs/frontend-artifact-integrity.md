# Frontend artifact integrity gate

Both forum targets call `scripts/build-forum-frontend.sh` before restarting any
workers. Deploy the application artifact checker and this Ops change together;
an older application revision without the checker is deliberately rejected.

The script installs from `pnpm-lock.yaml` using Corepack and the application's
pinned pnpm version. It uses a disposable source copy, empty `node_modules`, empty
`dist`, and a separate empty pnpm store for each build. Store/cache contents from
previous deployments cannot supply missing package build output. Dependency
lifecycle scripts are enabled; `--ignore-scripts` must not be used for Git packages
whose exported entry points are produced by `prepare`.

Before replacing output, it runs type checking, the production build, the
application's artifact integrity checker, SSR rendering smoke tests and auth
redirect checks. Any failure returns nonzero before touching the existing
`dist` or `node_modules`, so the deployer cannot proceed to worker restart.
Successful promotion retains the previous pair in `.frontend-previous.*` under
the forum root. These directories are rollback backups, not dependency caches;
operators can remove old pairs after confirming the deployment is healthy.
The temporary dependency store is removed on both success and failure.

No private registry is configured by this change. If a registry proxy is introduced,
its immutable package content must still pass the same checks; clearing a proxy
cache is not a substitute for validating the delivered artifact.

Run `bash scripts/test-build-forum-frontend.sh` to verify that failures during
install, type check, build, artifact validation and auth smoke leave live output
untouched, while a successful build promotes the validated dependency/output pair.
