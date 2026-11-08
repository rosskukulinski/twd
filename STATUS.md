# twd STATUS (living log)

## M0 — repo skeleton: DONE (2026-11-07)
- Layout per RFC §15: `cmd/{twd,twd-mgmt,twd-hostd,twd-netd}`, `netd/`, `mgmt/`, `hostd/`, `js/packages/{tailnet,mcp,create-twd}` (npm workspaces), `templates/`, `e2e/{local,real}`, `scripts/`, `internal/tsapi`.
- Go module `github.com/rosskukulinski/twd`, `tailscale.com v1.105.0-pre.0.20261107202901-503010831ecd` (= `.refs/tailscale` 5030108) from the public proxy, no `replace`. `GOTOOLCHAIN=local` (go1.27.3).
- workerd pinned exactly at `1.20261108.1` (root `package.json` devDependency, same as the prototype).
- Makefile: `test`, `e2e-local`, `e2e-real`, `lint`, `build`; `e2e-vm`/`release` are stubs that exit 1.
- Pre-commit secret hook installed into `.git/hooks`; `make lint` also runs `scripts/check-secrets.sh`.

## M1 — netd: IN PROGRESS
(see below once e2e-real has run)
