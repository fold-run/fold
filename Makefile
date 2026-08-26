# Single source of truth for dev/CI commands; CI calls these same targets.

.PHONY: build test race vet lint fmt fmt-check tidy-check vuln bench loadtest conformance sync-console console-check check fuzz cover helm-check compose-up compose-down compose-logs dev-up dev-down dev-status

build:
	go build ./...

test:
	go test ./...

race:
	go test -race ./...

# Race tests + coverage profile; CI uses this as its test step. Coverage is
# informational, not a gate. Inspect with: go tool cover -html=coverage.out
cover:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -func=coverage.out | tail -1

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

tidy-check:
	go mod tidy -diff

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

bench:
	FOLD_BENCH=1 go test ./bench -run TestAddedLatencyGate -v

# Throughput + tail latency sweep (docs/benchmarks.md). Not in CI: shared
# runners make throughput numbers noise; bench is the merge-time guard.
loadtest:
	go run ./tools/perf

# Seed corpora always run as part of `make test`; this explores further.
fuzz:
	go test ./config -run '^$$' -fuzz FuzzParse -fuzztime 30s
	go test ./gateway -run '^$$' -fuzz FuzzResolve -fuzztime 30s
	go test ./gateway -run '^$$' -fuzz FuzzListCursor -fuzztime 30s
	go test ./gateway -run '^$$' -fuzz FuzzDiscoveryDoc -fuzztime 30s

conformance:
	./scripts/conformance.sh

# Vendors gateway/console from fold-run/fold-console at CONSOLE_COMMIT.
sync-console:
	./scripts/sync-console.sh

# Proves gateway/console is exactly the pinned fold-console commit — the check
# that makes "the console is maintained elsewhere" true rather than a claim,
# since nothing else stops a hand edit here. Network, so it is its own CI job
# rather than part of `check` (same reasoning as conformance and helm-check).
console-check:
	./scripts/sync-console.sh
	@git diff --exit-code -- gateway/console gateway/console_source.go \
	  || { echo "gateway/console differed from its pin and has been re-vendored — review and commit"; exit 1; }
	@echo "gateway/console matches its pin"

# Lints the Helm chart and renders it against each ci values file. Not part
# of `check` (keeps the contributor toolchain Go-only); CI runs it in its own
# job. --api-versions lets the ServiceMonitor render without a cluster.
helm-check:
	helm lint deploy/helm/fold -f deploy/helm/fold/ci/default-values.yaml
	@for f in deploy/helm/fold/ci/*.yaml; do \
		echo "helm template -f $$f"; \
		helm template fold deploy/helm/fold -f $$f \
			--api-versions monitoring.coreos.com/v1 >/dev/null || exit 1; \
	done
	@if helm template fold deploy/helm/fold >/dev/null 2>&1; then \
		echo "expected render without a config to fail"; exit 1; \
	else echo "required-config guard OK"; fi

# Everything CI gates on except the bench and conformance jobs.
check: fmt-check tidy-check vet build race lint

# --- local dev stack (scripts/dev-stack.sh) ---------------------------------
# fold with a real MCP upstream behind it, for talking to the gateway you are
# working on: the Inspector, your editor, or a Claude Code session via the
# repo's .mcp.json. Go + node, no Docker. Ports are overridable (see the
# script); the gateway does NOT use its own default 8080, which is a popular
# squat.
dev-up:
	./scripts/dev-stack.sh up

dev-down:
	./scripts/dev-stack.sh down

dev-status:
	./scripts/dev-stack.sh status

# --- local compose stack (compose.yaml) ------------------------------------
# Not a gate; a one-command way to run the gateway on this host. PROFILES
# selects the optional services:
#   make compose-up PROFILES=                      # gateway only
#   make compose-up PROFILES="stdio observability" # default
#   make compose-up PROFILES="stdio redis observability"
PROFILES ?= stdio observability
COMPOSE = docker compose $(foreach p,$(PROFILES),--profile $(p))

# SHIM_TOKEN is written to .env rather than generated per invocation because
# the gateway and the shim must agree on it, and a fresh value on every `up`
# would leave the running shim rejecting the gateway until both restart.
compose-up:
	@test -f fold.config.json || { \
		echo "fold.config.json missing — cp fold.config.example.json fold.config.json (then edit)"; exit 1; }
	@test -f .env || { echo "SHIM_TOKEN=$$(openssl rand -hex 16)" > .env; echo "wrote .env with a fresh SHIM_TOKEN"; }
	@mkdir -p data
	$(COMPOSE) up -d
	@echo "health:     curl -fsS http://localhost:8080/health"
	@echo "prometheus: http://localhost:9090   grafana: http://localhost:3000"

compose-down:
	$(COMPOSE) down

compose-logs:
	$(COMPOSE) logs -f
