# twd build. GOTOOLCHAIN=local: use the installed Go (1.27.3), never download one.
export GOTOOLCHAIN := local
GO        ?= go
TS_CREDS  ?= $(HOME)/.config/tailnet-agents-design
TAILNET1  ?= tail7906d5.ts.net
WORKERD   := node_modules/.bin/workerd

.PHONY: all build deps js-build test test-go test-js test-pg e2e-local e2e-real e2e-vm guest-image release lint hooks clean gen-js test-m2mgmt

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

release:
	@echo "release: not implemented yet (milestone M6)"; exit 1

lint:
	@out=$$(gofmt -l $$(git ls-files -co --exclude-standard '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags e2e ./e2e/local/...
	$(GO) vet -tags e2ereal ./e2e/real/...
	$(GO) vet -tags m2mgmt ./e2e/m2mgmt/...
	GOOS=linux $(GO) vet ./hostd/vm/... ./guest/... ./cmd/twd-hostd/...
	GOOS=linux $(GO) vet -tags e2evm ./e2e/vm/...
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
