# twd build. GOTOOLCHAIN=local: use the installed Go (1.27.3), never download one.
export GOTOOLCHAIN := local
GO        ?= go
TS_CREDS  ?= $(HOME)/.config/tailnet-agents-design
TAILNET1  ?= tail7906d5.ts.net
WORKERD   := node_modules/.bin/workerd

.PHONY: all build deps test test-go test-js e2e-local e2e-real e2e-vm release lint hooks clean gen-js test-m2mgmt

all: build

build:
	$(GO) build -o bin/ ./cmd/...

$(WORKERD): package.json package-lock.json
	npm ci --no-audit --no-fund
	@touch $@

deps: $(WORKERD)
	$(GO) mod download

## test: hermetic unit + integration tests (testcontrol + local DERP/STUN + stock workerd).
test: deps test-go test-js

test-go:
	$(GO) test -race -count=1 ./netd/... ./internal/... ./cmd/... ./mgmt/... ./hostd/...

test-js:
	npm test --silent

## e2e-local: the twd-netd binary as a real process against a local control plane + workerd.
e2e-local: deps
	$(GO) test -tags e2e -count=1 -v ./e2e/local/...

## e2e-real: the identity/egress/kill suite on tailnet 1 (runs only if the creds exist).
e2e-real: deps
	@if [ ! -f "$(TS_CREDS)/tailnet1_api.env" ]; then \
		echo "e2e-real: SKIPPED ($(TS_CREDS)/tailnet1_api.env not found)"; exit 0; fi; \
	TWD_E2E_API_KEY_FILE="$(TS_CREDS)/tailnet1_api.env" TWD_E2E_TAILNET="$(TAILNET1)" \
		$(GO) test -tags e2ereal -count=1 -v -timeout 15m ./e2e/real/...

e2e-vm:
	@echo "e2e-vm: not implemented yet (milestone M8)"; exit 1

release:
	@echo "release: not implemented yet (milestone M6)"; exit 1

lint:
	@out=$$(gofmt -l $$(git ls-files -co --exclude-standard '*.go')); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags e2e ./e2e/local/...
	$(GO) vet -tags e2ereal ./e2e/real/...
	$(GO) vet -tags m2mgmt ./e2e/m2mgmt/...
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
