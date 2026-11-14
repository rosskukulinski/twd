# twd build. GOTOOLCHAIN=local: use the installed Go (1.27.3), never download one.
export GOTOOLCHAIN := local
GO        ?= go
TS_CREDS  ?= $(HOME)/.config/tailnet-agents-design
TAILNET1  ?= tail7906d5.ts.net
WORKERD   := node_modules/.bin/workerd

.PHONY: all build deps js-build api api-lint api-check test test-go test-js test-sdk test-pg e2e-local e2e-real e2e-mac e2e-multi e2e-keyless e2e-oauth e2e-mbp e2e-linux e2e-vm guest-image guest-kernel release lint hooks clean gen-js test-node vm-assets e2e-linux-vm e2e-linux-reboot vz-assets vz-kernel e2e-vz e2e-container test-images

all: build

## build: every binary into bin/. On macOS, twd-vz (the microVM tier's per-VM VMM, cgo on
## Virtualization.framework) is ad-hoc signed with the virtualization entitlement it needs.
VZ_ENTITLEMENTS := cmd/twd-vz/twd-vz.entitlements
build:
	$(GO) build -o bin/ ./cmd/...
	@if [ "$$(uname -s)" = Darwin ]; then codesign --force --sign - --entitlements $(VZ_ENTITLEMENTS) bin/twd-vz; fi

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
	$(GO) test -race -count=1 ./netd/... ./internal/... ./cmd/... ./mgmt/... ./hostd/... ./security/... ./guest/...

test-js:
	npm test --silent

## test-sdk: M5 SDK conformance (hermetic): real Cloudflare Agents SDK and Vercel AI SDK
## apps on twd-hostd + stock workerd over testcontrol + local DERP; mock LLM, no keys.
test-sdk: deps
	$(GO) test -tags sdk -count=1 -v -timeout 10m ./e2e/sdk/...

## test-images: ADR-032 D3 on the developer machine: twd deploy's image builder for real (docker
## buildx, both arches, alias contexts from twd's registry) and the registry conformance run with
## crane (CRANE=<path>, default: crane on PATH; go install github.com/google/go-containerregistry/cmd/crane@v0.20.6).
CRANE ?= $(shell command -v crane)
test-images:
	TWD_TEST_DOCKER=1 TWD_CRANE="$(CRANE)" $(GO) test -count=1 -v -run 'TestBuildAndPush|TestCraneConformance' ./internal/imagebuild/ ./internal/registry/

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

## guest-image / guest-kernel / vm-assets: the microVM guest (Linux only; reproducible; inputs
## pinned in guest/inputs.sh) for GUEST_ARCH (Go's name, amd64 or arm64; default: this machine).
## Either arch builds on either (cross: the amd64 kernel needs gcc-x86-64-linux-gnu on arm64,
## the arm64 one gcc-aarch64-linux-gnu on amd64).
## guest-image: the rootfs: workerd + twd-guestd + glibc (ubuntu-base 24.04) + a CA bundle.
GUEST_ARCH ?= $(shell $(GO) env GOARCH)
GUEST_OUT  ?= guest/out/$(GUEST_ARCH)
KERNEL_OUT ?= guest/out/kernel-$(GUEST_ARCH)
guest-image: $(WORKERD)
	TARGETARCH=$(GUEST_ARCH) guest/build-image.sh $(GUEST_OUT)
## guest-kernel: linux 6.1 LTS with Firecracker's CI guest config + guest/kernel/twd.config
## (erofs, overlayfs). Reused when its inputs are unchanged (KERNEL_OUT/kernel.json).
guest-kernel:
	TARGETARCH=$(GUEST_ARCH) guest/build-kernel.sh $(KERNEL_OUT)

## e2e-vm: the microVM tier in Firecracker (Linux + KVM, native GUEST_ARCH; run inside Lima
## twd-dev or on an amd64 KVM host; needs sudo). TWD_VM_KERNEL defaults to the guest-kernel
## build (set it to use another kernel, e.g. ~/fc/vmlinux, without building one).
DEFAULT_VM_KERNEL := $(abspath $(KERNEL_OUT))/vmlinux
TWD_VM_KERNEL ?= $(DEFAULT_VM_KERNEL)
VM_KERNEL_DEP = $(if $(filter $(DEFAULT_VM_KERNEL),$(TWD_VM_KERNEL)),guest-kernel)
# 2 MiB pages for guest RAM (runtime sysctl inside the Lima VM, not persisted):
# without them, workerd start-up under nested virt is ~8x slower.
VM_HUGEPAGES ?= 2048
# TWD_VM_VCPUS / TWD_VM_BOOTARGS (optional) pass through to the tests (sudo drops the env).
VM_ENV = PATH="$$PATH" TWD_VM_KERNEL="$(TWD_VM_KERNEL)" TWD_VM_ROOTFS="$(CURDIR)/$(GUEST_OUT)/rootfs.ext4" \
	$(if $(TWD_VM_VCPUS),TWD_VM_VCPUS="$(TWD_VM_VCPUS)") $(if $(TWD_VM_BOOTARGS),TWD_VM_BOOTARGS="$(TWD_VM_BOOTARGS)")
e2e-vm: deps guest-image $(VM_KERNEL_DEP)
	@[ "$$(uname -s)" = Linux ] || { echo "e2e-vm: Linux only; run it inside Lima twd-dev"; exit 1; }
	@[ "$(GUEST_ARCH)" = "$$($(GO) env GOARCH)" ] || { echo "e2e-vm: a $(GUEST_ARCH) guest can't boot on this $$($(GO) env GOARCH) host"; exit 1; }
	@[ "$$(cat /proc/sys/vm/nr_hugepages)" -ge $(VM_HUGEPAGES) ] || sudo sysctl -qw vm.nr_hugepages=$(VM_HUGEPAGES)
	$(GO) test -c -tags e2evm -o bin/e2evm.test ./e2e/vm
	$(GO) test -c -o bin/vmsmoke.test ./hostd/vm
	sudo env $(VM_ENV) bin/vmsmoke.test -test.v -test.count=1
	cd e2e/vm && sudo env $(VM_ENV) ../../bin/e2evm.test -test.v -test.count=1 -test.timeout 15m
## e2e-container: ADR-032 acceptance on Firecracker (Linux + KVM, in Lima twd-dev; needs sudo):
## the Sandbox SDK 0.x examples/minimal and the 1.0 flow run unmodified through hostd + workerd
## + one jailed VM per container, plus lifecycle/isolation checks and measurements.
## CTR_IMAGES is the dir guest/images/build-sandbox-images.sh wrote (run that on the Mac first);
## The guest kernel (TWD_VM_KERNEL, the guest-kernel build) has erofs, so containers boot it too.
CTR_IMAGES ?= $(CURDIR)/guest/out/images
CTR_KERNEL ?= $(TWD_VM_KERNEL)
CTR_APPS   ?= /tmp/twd-e2e-container-apps
CTR_ENV = $(VM_ENV) TWD_VM_CONTAINER_KERNEL="$(CTR_KERNEL)" TWD_CTR_IMAGES="$(CTR_IMAGES)" TWD_CTR_APPS="$(CTR_APPS)"
e2e-container: deps guest-image $(VM_KERNEL_DEP)
	@[ "$$(uname -s)" = Linux ] || { echo "e2e-container: Linux only; run it inside Lima twd-dev"; exit 1; }
	@[ -f "$(CTR_IMAGES)/images.json" ] || { echo "e2e-container: no $(CTR_IMAGES)/images.json (run guest/images/build-sandbox-images.sh on the Mac)"; exit 1; }
	@[ "$$(cat /proc/sys/vm/nr_hugepages)" -ge $(VM_HUGEPAGES) ] || sudo sysctl -qw vm.nr_hugepages=$(VM_HUGEPAGES)
	@[ -f "$(CTR_APPS)/unmodified.txt" ] || scripts/e2e-container-prep.sh $(CTR_APPS)
	$(GO) test -c -o bin/vmsmoke.test ./hostd/vm
	$(GO) test -c -tags e2econtainer -o bin/e2econtainer.test ./e2e/container
	sudo env $(CTR_ENV) bin/vmsmoke.test -test.v -test.count=1 -test.run TestContainerSmoke
	cd e2e/container && sudo env $(CTR_ENV) ../../bin/e2econtainer.test -test.v -test.count=1 -test.timeout 20m

## vm-assets: the microVM guest for the linux-$(GUEST_ARCH) release (Linux only; Lima twd-dev
## builds both arches): the guest image, the kernel (TWD_VM_KERNEL, + its kernel.json) and
## Firecracker + jailer (the pinned release, checksum-verified; FC_BIN_DIR=<dir> copies local
## binaries instead) in VM_ASSETS.
FC_BIN_DIR ?=
VM_ASSETS  ?= dist/vm-assets-$(GUEST_ARCH)
vm-assets: guest-image $(VM_KERNEL_DEP)
	@[ "$$(uname -s)" = Linux ] || { echo "vm-assets: Linux only; run it inside Lima twd-dev"; exit 1; }
	mkdir -p $(VM_ASSETS)
	cp -f $(GUEST_OUT)/rootfs.ext4 $(GUEST_OUT)/manifest.json $(VM_ASSETS)/
	cp -f $(TWD_VM_KERNEL) $(VM_ASSETS)/vmlinux
	@k="$$(dirname "$(TWD_VM_KERNEL)")/kernel.json"; if [ -f "$$k" ]; then cp -f "$$k" $(VM_ASSETS)/kernel.json; else rm -f $(VM_ASSETS)/kernel.json; fi
	@if [ -n "$(FC_BIN_DIR)" ]; then cp -f $(FC_BIN_DIR)/firecracker $(FC_BIN_DIR)/jailer $(VM_ASSETS)/; \
	else TARGETARCH=$(GUEST_ARCH) guest/fetch-firecracker.sh $(VM_ASSETS); fi
	node scripts/check-arch.mjs linux $(GUEST_ARCH) $(VM_ASSETS)/firecracker $(VM_ASSETS)/jailer
	@if [ "$(GUEST_ARCH)" = "$$($(GO) env GOARCH)" ]; then $(VM_ASSETS)/firecracker --version | head -1; fi

## vz-assets: the macOS microVM guest (vz backend): the arm64 guest kernel with virtiofs
## (guest/build-kernel-vz.sh) as vmlinux + the guest image, built in Lima twd-dev (or on arm64
## Linux) and cached in VZ_ASSETS by input hash (scripts/vz-assets-lima.sh).
VZ_ASSETS ?= dist/vz-assets-arm64
vz-assets:
	sh scripts/vz-assets-lima.sh $(VZ_ASSETS)

## vz-kernel: just the vz guest kernel (arm64 Linux only, e.g. inside Lima twd-dev) into bin/vz.
vz-kernel:
	guest/build-kernel-vz.sh bin/vz

## e2e-vz: the microVM tier on this Mac (Virtualization.framework; no sudo): the vz smoke,
## volume and Rosetta tests (hostd/vm), then E2E-VM (e2e/vm) with the vz backend: tier
## selection, attributed egress per object, DO storage on the encrypted volume, isolation,
## no keys in the guest, no host network from the guest, kill → restart, redeploy.
VZ_ENV = TWD_VZ_VMM="$(CURDIR)/bin/twd-vz" TWD_VM_KERNEL="$(CURDIR)/$(VZ_ASSETS)/vmlinux" TWD_VM_ROOTFS="$(CURDIR)/$(VZ_ASSETS)/rootfs.ext4"
e2e-vz: deps build vz-assets
	@[ "$$(uname -s)" = Darwin ] || { echo "e2e-vz: macOS only"; exit 1; }
	$(VZ_ENV) TWD_VM_ROSETTA=$$(bin/twd-vz -check $(VZ_ASSETS)/vmlinux | grep -q rosetta=installed && echo 1) \
		$(GO) test -count=1 -v -run 'TestVZ|TestDgram' ./hostd/vm
	cd e2e/vm && $(VZ_ENV) $(GO) test -tags e2evm -count=1 -v -timeout 15m .

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

## e2e-linux-reboot: L3 DO storage across host reboots of the same fresh VM (clean, then a
## power cut): helper back via systemd, twd up, same object node, data present, volume re-opens.
e2e-linux-reboot:
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-linux-reboot: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	TWD_E2E_RUN=TestE2ELinuxReboot TS_CREDS="$(TS_CREDS)" TAILNET1="$(TAILNET1)" sh scripts/e2e-linux-vm.sh $(LINUX_VM)

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

## e2e-keyless: M10 on tailnet 1: `twd up` keyless (WIF mint mode, mgmt gets NO API key; the key file
## is used only to mint the mgmt node's auth key, since a browser login can't be automated, and to
## create/delete the trust credentials), deploy, objects minted via WIF, `twd link` from a tsnet test
## client (tag:ttest, admin cap) → remote deploy/ls/kill/audit, a non-admin (tag:ttest-noadmin) refused.
e2e-keyless: deps
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-keyless: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	TWD_E2E_API_KEY_FILE="$(TS_CREDS)/tailnet1_api.env" TWD_E2E_TAILNET="$(TAILNET1)" \
		$(GO) test -tags e2ekeyless -count=1 -v -timeout 25m ./e2e/keyless/...

## e2e-oauth: M12 on tailnet 1: `twd up --oauth-client-file` (mint mode oauth: an OAuth client with
## auth_keys + devices:core + policy_file:read, tag:twd; no browser, no Funnel, no API key given to twd)
## → checks (a)–(f), `twd link` over https and http, remote deploy/kill/audit, zero leftovers. The API
## key creates and deletes the OAuth client, mints the test's own nodes and cleans up by ID.
e2e-oauth: deps
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-oauth: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	TWD_E2E_API_KEY_FILE="$(TS_CREDS)/tailnet1_api.env" TWD_E2E_TAILNET="$(TAILNET1)" \
		$(GO) test -tags e2eoauth -count=1 -v -timeout 25m ./e2e/oauth/...

## release: self-contained tarballs in dist/ (darwin-arm64, linux-arm64, linux-amd64): Go binaries
## (CGO_ENABLED=0, esbuild built in), pinned workerd, templates + vendored node_modules,
## install.sh, VERSION, SHA256SUMS. darwin binaries are ad-hoc signed (run on macOS).
# Expanded once (:=): a recursive variable would be re-evaluated per use, so an
# edit during `make release` would name the e2e tarball differently.
ifndef RELEASE_VERSION
RELEASE_VERSION := $(shell cat VERSION)-$(shell git rev-parse --short=8 HEAD)$(shell git diff --quiet HEAD -- . ':!STATUS.md' || echo -dirty)
endif
RELEASE_TARGETS ?= darwin-arm64 linux-arm64 linux-amd64
## The linux-arm64 and linux-amd64 microVM guests (share/twd/vm: kernel, rootfs, firecracker,
## jailer) are built by scripts/vm-assets-lima.sh: on Linux directly, on macOS inside Lima
## twd-dev (both arches there; cached in dist/vm-assets-<arch> by input hash).
## RELEASE_VM_ASSETS=auto (default): include them when a builder is available, else warn and
## ship without the microVM tier (release.json "microvm": false); 1: required; 0: never.
## TWD_VM_ASSETS_arm64 / TWD_VM_ASSETS_amd64 override.
## The darwin-arm64 microVM guest (share/twd/vm: the vz kernel, the same rootfs) comes from
## scripts/vz-assets-lima.sh the same way (TWD_VZ_ASSETS overrides); its bin/twd-vz is
## signed with the virtualization entitlement.
RELEASE_VM_ASSETS ?= auto
release: deps
	rm -rf dist/stage dist/twd-*.tar.gz dist/SHA256SUMS
	@for a in arm64 amd64; do \
		eval "va=\$${TWD_VM_ASSETS_$$a:-}"; \
		if [ -z "$$va" ] && [ "$(RELEASE_VM_ASSETS)" != 0 ] && echo "$(RELEASE_TARGETS)" | grep -q "linux-$$a"; then \
			if sh scripts/vm-assets-lima.sh dist/vm-assets-$$a $$a; then va="$(CURDIR)/dist/vm-assets-$$a"; \
			elif [ "$(RELEASE_VM_ASSETS)" = 1 ]; then echo "release: linux-$$a microVM assets required (RELEASE_VM_ASSETS=1)"; exit 1; \
			else echo "release: WARNING: the linux-$$a tarball will have NO microVM guest (see scripts/vm-assets-lima.sh)"; fi; \
		fi; \
		eval "TWD_VM_ASSETS_$$a=\$$va"; export TWD_VM_ASSETS_$$a; \
	done; \
	vz="$${TWD_VZ_ASSETS:-}"; \
	if [ -z "$$vz" ] && [ "$(RELEASE_VM_ASSETS)" != 0 ] && echo "$(RELEASE_TARGETS)" | grep -q darwin-arm64; then \
		if sh scripts/vz-assets-lima.sh $(VZ_ASSETS); then vz="$(CURDIR)/$(VZ_ASSETS)"; \
		elif [ "$(RELEASE_VM_ASSETS)" = 1 ]; then echo "release: microVM assets required (RELEASE_VM_ASSETS=1)"; exit 1; \
		else echo "release: WARNING: the darwin-arm64 tarball will have NO microVM guest (see scripts/vz-assets-lima.sh)"; fi; \
	fi; \
	TWD_VZ_ASSETS="$$vz" sh scripts/release.sh $(RELEASE_VERSION) $(RELEASE_TARGETS)

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

## e2e-linux: E2E-LINUX: the e2e-mbp checks (a)–(f) + twd doctor + `twd up --service`
## (systemd user unit; survives `systemctl --user restart`) on FRESH Lima VMs
## twdl-<distro> that mount nothing from this Mac, from the linux release tarballs.
## twd runs in the VM; the test client + echo run here. ≤2 VMs at a time; each is
## stopped (kept) when its row is done. LINUX_DISTROS picks rows (see
## scripts/e2e-linux-one.sh; debian11-arm64 / alpine-arm64 check doctor's refusal).
LINUX_DISTROS ?= ubuntu24-amd64 ubuntu24-arm64 debian12-arm64 ubuntu22-arm64
e2e-linux: release
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-linux: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	if ! command -v limactl >/dev/null; then echo "e2e-linux: SKIPPED (no limactl)"; exit 0; fi; \
	TWD_E2E_API_KEY_FILE="$(TS_CREDS)/tailnet1_api.env" TWD_E2E_TAILNET="$(TAILNET1)" \
		TWD_E2E_TARBALL_arm64="$(CURDIR)/dist/twd-$(RELEASE_VERSION)-linux-arm64.tar.gz" \
		TWD_E2E_TARBALL_amd64="$(CURDIR)/dist/twd-$(RELEASE_VERSION)-linux-amd64.tar.gz" \
		sh scripts/e2e-linux.sh $(LINUX_DISTROS)

## api: regenerate everything derived from api/openapi.yaml (the /v1 source of truth):
## Go server interfaces + types + embedded spec (mgmt/api/v1), the Go client
## (hostd/mgmtapi/v1) and docs/contracts/mgmt-api.md. oapi-codegen is pinned as a
## go.mod tool; nothing here needs cgo or a network once modules are cached.
api:
	$(GO) tool oapi-codegen -config api/oapi-codegen.server.yaml api/openapi.yaml
	$(GO) tool oapi-codegen -config api/oapi-codegen.client.yaml api/openapi.yaml
	$(GO) run ./api/render -in api/openapi.yaml -out docs/contracts/mgmt-api.md

## api-lint: Redocly lint of the spec (pinned version, via npx; warnings fail: see redocly.yaml).
REDOCLY ?= npx -y @redocly/cli@2.76.0
api-lint:
	$(REDOCLY) lint api/openapi.yaml

## api-check: CI gate: the generated code and the rendered contract are current.
api-check: api
	@if ! git diff --quiet --exit-code -- mgmt/api/v1 hostd/mgmtapi/v1 docs/contracts/mgmt-api.md; then \
		echo "api-check: generated files are stale; run 'make api' and commit:"; \
		git --no-pager diff --stat -- mgmt/api/v1 hostd/mgmtapi/v1 docs/contracts/mgmt-api.md; exit 1; fi
	$(GO) test -count=1 ./mgmt/api/...

lint: api-check
	@out=$$(gofmt -l $$(git ls-files -co --exclude-standard '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags e2e ./e2e/local/...
	$(GO) vet -tags e2ereal ./e2e/real/...
	GOOS=linux $(GO) vet ./hostd/vm/... ./guest/... ./cmd/twd-hostd/... ./cmd/twd-vmhelper/... ./cmd/twd/...
	GOOS=linux $(GO) vet -tags e2evm ./e2e/vm/...
	GOOS=linux $(GO) vet -tags e2econtainer ./e2e/container/...
	GOOS=darwin $(GO) vet -tags e2evm ./e2e/vm/...
	$(GO) vet -tags e2elinuxvm ./e2e/linuxvm/...
	$(GO) vet -tags e2emac ./e2e/mac/...
	$(GO) vet -tags e2emulti ./e2e/multi/...
	$(GO) vet -tags e2ekeyless ./e2e/keyless/...
	$(GO) vet -tags e2eoauth ./e2e/oauth/...
	$(GO) vet -tags e2embp ./e2e/mbp/...
	$(GO) vet -tags e2elinux ./e2e/linux/...
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

## test-node: the opt-in Node process tier + Vercel Workflow (hermetic; npm ci of the pinned deps in e2e/node).
test-node: deps
	cd e2e/node && npm ci --no-audit --no-fund --loglevel=error
	npm test -w @twd/node --silent
	$(GO) test -tags nodetier -count=1 -v -timeout 10m ./e2e/node/...
