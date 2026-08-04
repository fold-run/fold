# Single source of truth for dev/CI commands; CI calls these same targets.

.PHONY: build test race vet lint fmt fmt-check tidy-check vuln bench conformance check fuzz

build:
	go build ./...

test:
	go test ./...

race:
	go test -race ./...

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

# Everything CI gates on except the bench and conformance jobs.
check: fmt-check tidy-check vet build race lint
