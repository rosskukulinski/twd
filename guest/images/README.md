# Container images for arm64 hosts (ADR-032 D3)

Cloudflare publishes `docker.io/cloudflare/sandbox` for **linux/amd64 only**.
On arm64 twd hosts, `hostd/image.DefaultAliases` swaps in twd's own arm64
rebuilds, so app code and Dockerfiles stay unchanged:

| ref the app names | arm64 host resolves to |
|---|---|
| `docker.io/cloudflare/sandbox:0.12.14` | `twd.local/cloudflare-sandbox:0.12.14-arm64` |
| `docker.io/cloudflare/sandbox:1.0.1` | `twd.local/cloudflare-sandbox:1.0.1-arm64` |

amd64 hosts use the stock images. The alias applies in two places:

- **at deploy** (mgmt, `internal/registry.Resolver`): the stock ref is pinned to
  the rebuild in twd's registry (`public/cloudflare-sandbox`, pushed by the
  operator with `twd image push`) for arm64 hosts, to the mirrored stock image
  for amd64 ones; hosts without a registry still alias in `image.Store.Resolve`;
- **at build time** (`twd deploy`, `internal/imagebuild`): a Dockerfile
  `FROM docker.io/cloudflare/sandbox:<v>` (or `COPY --from=` it, or an ARG
  default naming it) is built for arm64 with a BuildKit named context
  (`--build-context docker.io/cloudflare/sandbox:<v>=oci-layout://…`, the
  rebuild fetched from twd's registry or the local Docker store), so the app's
  Dockerfile is used as written. (This script still uses `docker-image://`
  contexts on images it has just loaded.)

## How the rebuilds are produced

`guest/images/build-sandbox-images.sh OUT_DIR` (on the developer machine;
macOS + OrbStack or Linux + docker buildx). It clones
github.com/cloudflare/sandbox-sdk (Apache-2.0) into a fresh temp dir, checks
that each release tag points at the pinned commit, and builds only its own
Dockerfiles inside BuildKit (the clone is treated as data):

| image | source | build |
|---|---|---|
| `cloudflare-sandbox:0.12.14-arm64` | tag `@cloudflare/sandbox@0.12.14` = `b96de6e` | `packages/sandbox/Dockerfile`, target `default`, `SANDBOX_VERSION=0.12.14`, `BUN_VERSION` from `.bun-version` (1.4.4), `NODE_VERSION=22.23.4` (the tag's `docker-bake.hcl`), `--platform linux/arm64` |
| `cloudflare-sandbox:1.0.0-arm64` | tag `@cloudflare/sandbox@1.0.0` = `f9e972a` | `images/sandbox-tools/Dockerfile`, target `image` (the static `sandbox-shim`) |
| `cloudflare-sandbox:1.0.1-arm64` | tag `@cloudflare/sandbox@1.0.1` = `8a535b3` | same, at 1.0.1 |

**The one source change** (0.12.14 only): `packages/sandbox-container/build.ts`
hard-codes `bun build --compile --target=bun-linux-x64` (and `-x64-musl`) for
the control server binary `/container-server/sandbox`, so an arm64 build of
the Dockerfile still ships an x86-64 binary (`exec format error` in the
guest). The script rewrites the target to `bun-linux-arm64` before building.
The 1.0.x shim Dockerfile's `verify` stage asserts x86-64, so the script
builds the `image` target (which `verify` does not gate) directly; the shim
is a static Rust binary built natively for the platform.

Everything the Dockerfiles download is pinned upstream (cloudflared,
fuse-overlayfs, python-build-standalone by sha256; base images by tag). The
script also builds the e2e images (`Dockerfile.debian-trixie-python`: the
raw-exec flow's image, Debian trixie + python3 + the 1.0.1 shim, standing in
for Cloudflare's non-public `cloudflare/debian-trixie`; and the two upstream
`examples/minimal` Dockerfiles, unmodified, with the aliases as named
contexts), `docker save`s every image to `OUT_DIR`, and writes
`OUT_DIR/images.json` (the index `image.DirSource` reads) and `SHA256SUMS`.

Built 2026-11-13 on macOS (OrbStack, BuildKit 0.33.1): the 0.12.14 rebuild is
~250 MB as a tarball and 569 MiB as erofs; the shims are 1.6 MB.

## Licence

The sandbox-sdk sources are Apache-2.0 (© Cloudflare, Inc.); the rebuilds are
redistributed under the same licence, with this file as the notice of the
change above.
