.PHONY: build check-economy check-gate check-retrieval clean corpora coverage deps fmt fmt-check help install lint mod-check quality race run-ask run-bench run-explain run-pack run-scan run-stats run-ui scan-self test test-short vet vuln

BINARY   := neurofs
CMD_PATH := ./cmd/neurofs
OUT_DIR  := ./bin
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
TOOLS_DIR := $(CURDIR)/.tools

COVERAGE_MIN ?= 60.0
COVERAGE_OUT ?= coverage.out
GOLANGCI_LINT_VERSION ?= v2.12.2
GOVULNCHECK_VERSION ?= v1.1.4
GOLANGCI_LINT := $(TOOLS_DIR)/golangci-lint
GOLANGCI_LINT_STAMP := $(TOOLS_DIR)/.golangci-lint-$(GOLANGCI_LINT_VERSION)

## deps: Download and tidy Go module dependencies
deps:
	go mod tidy

## build: Compile the neurofs binary to ./bin/neurofs
build:
	@mkdir -p $(OUT_DIR)
	@source_hash="$$(go run $(CMD_PATH) g5-source-hash --repo "$(CURDIR)")"; \
	  if [ "$${#source_hash}" -ne 64 ]; then \
	    echo "invalid G5 source hash: $$source_hash" >&2; exit 1; \
	  fi; \
	  go build -ldflags "-X github.com/Gere2/neurofs/internal/cli.Version=$(VERSION) -X github.com/Gere2/neurofs/internal/gate.BuildSourceTreeSHA256=$$source_hash" -o $(OUT_DIR)/$(BINARY) $(CMD_PATH)
	@echo "built: $(OUT_DIR)/$(BINARY) version $(VERSION)"

## install: Install neurofs to GOPATH/bin (makes it available system-wide)
install:
	@source_hash="$$(go run $(CMD_PATH) g5-source-hash --repo "$(CURDIR)")"; \
	  if [ "$${#source_hash}" -ne 64 ]; then \
	    echo "invalid G5 source hash: $$source_hash" >&2; exit 1; \
	  fi; \
	  go install -ldflags "-X github.com/Gere2/neurofs/internal/cli.Version=$(VERSION) -X github.com/Gere2/neurofs/internal/gate.BuildSourceTreeSHA256=$$source_hash" $(CMD_PATH)

## test: Run all tests
test:
	go test ./... -v -count=1

## test-short: Run tests skipping integration tests
test-short:
	go test ./... -short -count=1

## race: Run the full test suite with the race detector
race:
	go test -race -count=1 ./...

## coverage: Run cross-package coverage and enforce the current no-regression floor
coverage:
	go test -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$(COVERAGE_OUT)" ./...
	@total=$$(go tool cover -func="$(COVERAGE_OUT)" | awk '/^total:/ { gsub("%", "", $$3); print $$3 }'); \
	  printf 'total coverage: %s%% (minimum %s%%)\n' "$$total" "$(COVERAGE_MIN)"; \
	  awk -v total="$$total" -v min="$(COVERAGE_MIN)" 'BEGIN { if (total + 0 < min + 0) exit 1 }'

## clean: Remove build artefacts
clean:
	rm -rf ./bin "$(CURDIR)/.tools"
	rm -f ./coverage.out

## run-ui: Start the local UI against the current directory (recommended entry point)
run-ui: build
	$(OUT_DIR)/$(BINARY) ui

## run-scan: Index the sample repository (useful for quick smoke-testing)
run-scan: build
	$(OUT_DIR)/$(BINARY) scan ./testdata/sample-repo -v

## run-ask: Ask a question against the sample repository
run-ask: build
	$(OUT_DIR)/$(BINARY) ask "how does authentication work?" \
	  --repo ./testdata/sample-repo \
	  --budget 4000 \
	  --format markdown

## run-pack: Export a bundle from the sample repository
run-pack: build
	$(OUT_DIR)/$(BINARY) pack "how does authentication work?" \
	  --repo ./testdata/sample-repo \
	  --budget 4000 \
	  --out /tmp/auth-context.prompt
	@echo "bundle written to /tmp/auth-context.prompt"

## run-stats: Show index metrics for the sample repository
run-stats: build
	$(OUT_DIR)/$(BINARY) stats --repo ./testdata/sample-repo

## run-explain: Ask with full scoring table
run-explain: build
	$(OUT_DIR)/$(BINARY) ask "how does authentication work?" \
	  --repo ./testdata/sample-repo \
	  --budget 4000 \
	  --explain \
	  >/dev/null

## run-bench: Run the retrieval-precision benchmark against the sample repo
run-bench: build
	$(OUT_DIR)/$(BINARY) bench --repo ./testdata/sample-repo --min-top3 75

## vet: Run go vet
vet:
	go vet ./...

## fmt: Format all Go files
fmt:
	@git ls-files --cached --others --exclude-standard -z -- '*.go' | \
	  xargs -0 sh -c 'for file do [ ! -f "$$file" ] || gofmt -w "$$file"; done' sh

## fmt-check: Fail when repository Go code is not gofmt-clean
fmt-check:
	@unformatted=$$(git ls-files --cached --others --exclude-standard -z -- '*.go' | \
	  xargs -0 sh -c 'for file do [ ! -f "$$file" ] || gofmt -l "$$file"; done' sh); \
	  if [ -n "$$unformatted" ]; then \
	    printf 'gofmt required for:\n%s\n' "$$unformatted"; \
	    exit 1; \
	  fi

## mod-check: Fail when go.mod or go.sum is not tidy
mod-check:
	go mod tidy -diff

$(GOLANGCI_LINT_STAMP):
	@mkdir -p "$(TOOLS_DIR)"
	GOBIN="$(TOOLS_DIR)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@touch "$@"

## lint: Run a repository-pinned golangci-lint (bootstraps into .tools/)
lint: $(GOLANGCI_LINT_STAMP)
	"$(GOLANGCI_LINT)" run ./...

## vuln: Check reachable Go vulnerabilities with the pinned scanner
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

## quality: Run deterministic source, module, lint, vet, race, coverage and vulnerability gates
quality: fmt-check mod-check lint vet race coverage vuln

## scan-self: Build and index this repo with deterministic local embeddings
scan-self: build
	NEUROFS_EMBEDDING_PROVIDER=mock $(OUT_DIR)/$(BINARY) scan .

## check-retrieval: Recall, precision, deterministic stability and token ceilings
check-retrieval: scan-self
	NEUROFS_EMBEDDING_PROVIDER=mock $(OUT_DIR)/$(BINARY) learn eval --min-recall 0.80
	NEUROFS_EMBEDDING_PROVIDER=mock $(OUT_DIR)/$(BINARY) bench \
	  --bundle --pack-budget 4000 --prefer-signatures \
	  --search --context --search-stability \
	  --min-top3 60 --min-search-top3 70 --min-context-top3 65 \
	  --min-search-stability 100 \
	  --max-mean-bundle-tokens 4000 \
	  --max-mean-search-tokens 1500 \
	  --max-mean-context-tokens 2750

## check-economy: Enforce the 25% iso-recall token-reduction decision threshold
check-economy: scan-self
	NEUROFS_EMBEDDING_PROVIDER=mock $(OUT_DIR)/$(BINARY) economy --gate --threshold 0.25 --search-limit 8

## check-gate: Run the full pivot-readiness oracle without changing its criteria
check-gate: scan-self
	NEUROFS_EMBEDDING_PROVIDER=mock $(OUT_DIR)/$(BINARY) gate

## g5-remeasure: Re-measure the G5 cross-shape evidence from a clean clone (run after any commit that touches code, docs or deps)
g5-remeasure:
	scripts/g5_remeasure.sh $(TAG)

## g5-verify: Verify the committed G5 evidence the way CI does, from a fresh clone
g5-verify:
	scripts/g5_verify.sh

## corpora: Clone and index the cross-shape tuning corpora (pallets/click, vuejs/core) under /tmp — required before any multi-corpus `learn tune`; /tmp is wiped on reboot so re-run as needed
corpora: build
	@test -d /tmp/click || git clone --depth 1 https://github.com/pallets/click /tmp/click
	@test -d /tmp/vue || git clone --depth 1 https://github.com/vuejs/core /tmp/vue
	$(OUT_DIR)/$(BINARY) scan /tmp/click
	$(OUT_DIR)/$(BINARY) scan /tmp/vue
	@echo ""
	@echo "cross-shape tune (chunk search):"
	@echo "  $(OUT_DIR)/$(BINARY) learn tune --corpus /tmp/click:docs/g5_fixtures/click --corpus /tmp/vue:docs/g5_fixtures/vue"
	@echo "cross-shape tune (file ranker):"
	@echo "  $(OUT_DIR)/$(BINARY) learn tune-files --bench /tmp/click:docs/g5_bench/click.json --bench /tmp/vue:docs/g5_bench/vue.json"

## help: Print available targets
help:
	@grep -E '^##' Makefile | sed 's/## //'
