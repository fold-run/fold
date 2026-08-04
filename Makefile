# Single source of truth for dev/CI commands; CI calls these same targets.

.PHONY: build test race vet lint fmt fmt-check tidy-check vuln bench conformance check fuzz cover helm-check

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

# Seed corpora always run as part of `make test`; this explores further.
fuzz:
	go test ./config -run '^$$' -fuzz FuzzParse -fuzztime 30s
	go test ./gateway -run '^$$' -fuzz FuzzResolve -fuzztime 30s

conformance:
	./scripts/conformance.sh

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
