# twd build. GOTOOLCHAIN=local: use the installed Go (1.27.3), never download one.
export GOTOOLCHAIN := local
GO        ?= go
TS_CREDS  ?= $(HOME)/.config/tailnet-agents-design
TAILNET1  ?= tail7906d5.ts.net
WORKERD   := node_modules/.bin/workerd

.PHONY: all build deps js-build test test-go test-js test-sdk test-pg e2e-local e2e-real e2e-mac e2e-multi e2e-mbp e2e-vm guest-image release lint hooks clean gen-js test-m2mgmt test-node vm-assets e2e-linux-vm

all: build

build:
	$(GO) build -o bin/ ./cmd/...

$(WORKERD): package.json package-lock.json
	npm ci --no-audit --no-fund
	@touch $@

deps: $(WORKERD) js-build
	$(GO) mod download

## js-build: compile the TypeScript packages (dist/ is gitignored, so clean clones need this).
js-build: $(WORKERD)
	npm run -w @twd/tailnet build
	npm run -w @twd/mcp build
	npm run --workspaces --if-present build

## test: hermetic unit + integration tests (testcontrol + local DERP/STUN + stock workerd).
test: deps test-go test-js

test-go:
	$(GO) test -race -count=1 ./netd/... ./internal/... ./cmd/... ./mgmt/... ./hostd/...

test-js:
	npm test --silent

## test-sdk: M5 SDK conformance (hermetic): real Cloudflare Agents SDK and Vercel AI SDK
## apps on twd-hostd + stock workerd over testcontrol + local DERP; mock LLM, no keys.
test-sdk: deps
	$(GO) test -tags sdk -count=1 -v -timeout 10m ./e2e/sdk/...

## test-pg: the mgmt Store tests against Postgres 17 in OrbStack docker (throwaway container).
PG_CTX  ?= orbstack
PG_PORT ?= 55433
test-pg:
	sh scripts/test-pg.sh "$(PG_CTX)" "$(PG_PORT)"

## e2e-local: the twd-netd binary as a real process against a local control plane + workerd.
e2e-local: deps
	$(GO) test -tags e2e -count=1 -v ./e2e/local/...

## e2e-real: the identity/egress/kill suite on tailnet 1 (runs only if the creds exist).
e2e-real: deps
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-real: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	TWD_E2E_API_KEY_FILE="$(TS_CREDS)/tailnet1_api.env" TWD_E2E_TAILNET="$(TAILNET1)" \
		$(GO) test -tags e2ereal -count=1 -v -timeout 15m ./e2e/real/...

## guest-image: the microVM guest rootfs (Linux only; reproducible): workerd + twd-guestd.
GUEST_OUT ?= guest/out
guest-image: $(WORKERD)
	guest/build-image.sh $(GUEST_OUT)

## e2e-vm: the microVM tier in Firecracker (Linux + KVM; run inside Lima twd-dev, needs sudo).
TWD_VM_KERNEL ?= $(HOME)/fc/vmlinux
# 2 MiB pages for guest RAM (runtime sysctl inside the Lima VM, not persisted):
# without them, workerd start-up under nested virt is ~8x slower.
VM_HUGEPAGES ?= 2048
VM_ENV = PATH="$$PATH" TWD_VM_KERNEL="$(TWD_VM_KERNEL)" TWD_VM_ROOTFS="$(CURDIR)/$(GUEST_OUT)/rootfs.ext4"
e2e-vm: deps guest-image
	@[ "$$(uname -s)" = Linux ] || { echo "e2e-vm: Linux only; run it inside Lima twd-dev"; exit 1; }
	@[ "$$(cat /proc/sys/vm/nr_hugepages)" -ge $(VM_HUGEPAGES) ] || sudo sysctl -qw vm.nr_hugepages=$(VM_HUGEPAGES)
	$(GO) test -c -tags e2evm -o bin/e2evm.test ./e2e/vm
	$(GO) test -c -o bin/vmsmoke.test ./hostd/vm
	sudo env $(VM_ENV) bin/vmsmoke.test -test.v -test.count=1
	cd e2e/vm && sudo env $(VM_ENV) ../../bin/e2evm.test -test.v -test.count=1 -test.timeout 15m
## vm-assets: the microVM guest for the linux release (Linux only, in Lima twd-dev): the guest
## image, the kernel (TWD_VM_KERNEL) and Firecracker + jailer (FC_BIN_DIR) in VM_ASSETS.
FC_BIN_DIR ?= /usr/local/bin
VM_ASSETS  ?= dist/vm-assets-$(shell go env GOARCH)
vm-assets: guest-image
	@[ "$$(uname -s)" = Linux ] || { echo "vm-assets: Linux only; run it inside Lima twd-dev"; exit 1; }
	mkdir -p $(VM_ASSETS)
	cp -f $(GUEST_OUT)/rootfs.ext4 $(GUEST_OUT)/manifest.json $(VM_ASSETS)/
	cp -f $(TWD_VM_KERNEL) $(VM_ASSETS)/vmlinux
	cp -f $(FC_BIN_DIR)/firecracker $(FC_BIN_DIR)/jailer $(VM_ASSETS)/
	$(VM_ASSETS)/firecracker --version | head -1

## e2e-linux-vm: L3 E2E from the installed release on a FRESH nested-virt Lima VM
## (twdl-kvm-u24; created if missing, stopped at the end): the linux-arm64 release with the
## microVM assets is built in Lima twd-dev, copied in with limactl copy, then install.sh ->
## sudo twd host setup --microvm -> twd up -> agent + mcp with tier microvm -> checks (a)-(f) from
## this Mac over tailnet 1, storage persistence across a VM kill and a redeploy, no keys in
## the guest, twd host setup --undo.
LINUX_VM ?= twdl-kvm-u24
e2e-linux-vm:
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-linux-vm: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	TS_CREDS="$(TS_CREDS)" TAILNET1="$(TAILNET1)" sh scripts/e2e-linux-vm.sh $(LINUX_VM)

## e2e-mac: E2E-MAC via the twd CLI on tailnet 1 (twd up → deploy agent+mcp → checks a–f → twd down → zero leftovers).
e2e-mac: deps
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-mac: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	TWD_E2E_API_KEY_FILE="$(TS_CREDS)/tailnet1_api.env" TWD_E2E_TAILNET="$(TAILNET1)" \
		$(GO) test -tags e2emac -count=1 -v -timeout 20m ./e2e/mac/...

## e2e-multi: E2E-MULTI on this Mac: one host serves tenant A (tailnet 1, `twd up`) and tenant B
## (tailnet-2, `twd tenant add`, name discovered) with the same agent app; isolation checks a–f;
## zero leftovers in both tailnets. Runs only if both credential files exist.
e2e-multi: deps
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ] || [ ! -f "$(TS_CREDS)/tailnet2_api.env" ]; then \
		echo "e2e-multi: SKIPPED (needs $(TS_CREDS)/tailnet1_api.env and tailnet2_api.env)"; exit 0; fi; \
	TWD_E2E_API_KEY_FILE="$(TS_CREDS)/tailnet1_api.env" TWD_E2E_API_KEY_FILE_B="$(TS_CREDS)/tailnet2_api.env" \
		TWD_E2E_TAILNET="$(TAILNET1)" \
		$(GO) test -tags e2emulti -count=1 -v -timeout 25m ./e2e/multi/...

## release: self-contained tarballs in dist/ (darwin-arm64 + linux-arm64): Go binaries
## (CGO_ENABLED=0, esbuild built in), pinned workerd, templates + vendored node_modules,
## install.sh, VERSION, SHA256SUMS. darwin binaries are ad-hoc signed (run on macOS).
RELEASE_VERSION ?= $(shell cat VERSION)-$(shell git rev-parse --short=8 HEAD)$(shell git diff --quiet HEAD -- . ':!STATUS.md' || echo -dirty)
RELEASE_TARGETS ?= darwin-arm64 linux-arm64
release: deps
	rm -rf dist/stage dist/twd-*.tar.gz dist/SHA256SUMS
	sh scripts/release.sh $(RELEASE_VERSION) $(RELEASE_TARGETS)

## e2e-mbp: E2E-MAC on the clean MacBook `mbp` from the release tarball (skips without
## the tailnet-1 key file or `ssh mbp`). twd runs on mbp; the test client + echo run here.
MBP ?= mbp
e2e-mbp: release
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-mbp: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	if ! ssh -o BatchMode=yes -o ConnectTimeout=10 $(MBP) true 2>/dev/null; then \
		echo "e2e-mbp: SKIPPED (ssh $(MBP) not reachable)"; exit 0; fi; \
	TWD_E2E_API_KEY_FILE="$(TS_CREDS)/tailnet1_api.env" TWD_E2E_TAILNET="$(TAILNET1)" TWD_E2E_SSH_HOST="$(MBP)" \
		TWD_E2E_TARBALL="$(CURDIR)/dist/twd-$(RELEASE_VERSION)-darwin-arm64.tar.gz" \
		sh scripts/e2e-mbp.sh $(MBP) -tags e2embp -count=1 -v -timeout 25m ./e2e/mbp/...

lint:
	@out=$$(gofmt -l $$(git ls-files -co --exclude-standard '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags e2e ./e2e/local/...
	$(GO) vet -tags e2ereal ./e2e/real/...
	$(GO) vet -tags m2mgmt ./e2e/m2mgmt/...
	GOOS=linux $(GO) vet ./hostd/vm/... ./guest/... ./cmd/twd-hostd/... ./cmd/twd-vmhelper/... ./cmd/twd/...
	GOOS=linux $(GO) vet -tags e2evm ./e2e/vm/...
	$(GO) vet -tags e2elinuxvm ./e2e/linuxvm/...
	$(GO) vet -tags e2emac ./e2e/mac/...
	$(GO) vet -tags e2emulti ./e2e/multi/...
	$(GO) vet -tags e2embp ./e2e/mbp/...
	$(GO) vet -tags sdk ./e2e/sdk/...
	$(GO) vet -tags nodetier ./e2e/node/...
	@sh scripts/check-secrets.sh

hooks:
	sh scripts/install-hooks.sh

clean:
	rm -rf bin dist

## gen-js: rebuild the runtime module hostd embeds into every deployment (from @twd/tailnet).
gen-js: $(WORKERD)
	node_modules/.bin/esbuild js/packages/tailnet/src/runtime.ts --bundle --format=esm --platform=neutral \
		--target=es2022 --legal-comments=none --log-level=warning --outfile=hostd/gen/js/twd-rt.js

## test-m2mgmt: hostd against the real twd-mgmt built from origin/m2-mgmt (hermetic).
test-m2mgmt: deps
	rm -rf /tmp/twd-m2src && mkdir -p /tmp/twd-m2src && git fetch -q origin m2-mgmt && git archive origin/m2-mgmt | tar -x -C /tmp/twd-m2src
	cd /tmp/twd-m2src && $(GO) build -o /tmp/twd-m2src/twd-mgmt ./cmd/twd-mgmt
	TWD_MGMT_BIN=/tmp/twd-m2src/twd-mgmt $(GO) test -tags m2mgmt -count=1 -v -timeout 5m ./e2e/m2mgmt/...

## test-node: the opt-in Node process tier + Vercel Workflow (hermetic; npm ci of the pinned deps in e2e/node).
test-node: deps
	cd e2e/node && npm ci --no-audit --no-fund --loglevel=error
	npm test -w @twd/node --silent
	$(GO) test -tags nodetier -count=1 -v -timeout 10m ./e2e/node/...
