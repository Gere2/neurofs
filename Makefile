.PHONY: check-retrieval build test clean install run-scan run-ask run-pack run-stats run-bench run-explain run-ui deps lint

BINARY   := neurofs
CMD_PATH := ./cmd/neurofs
OUT_DIR  := ./bin
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

## deps: Download and tidy Go module dependencies
deps:
	go mod tidy

## build: Compile the neurofs binary to ./bin/neurofs
build:
	@mkdir -p $(OUT_DIR)
	go build -ldflags "-X github.com/neuromfs/neuromfs/internal/cli.Version=$(VERSION)" -o $(OUT_DIR)/$(BINARY) $(CMD_PATH)
	@echo "built: $(OUT_DIR)/$(BINARY) version $(VERSION)"

## install: Install neurofs to GOPATH/bin (makes it available system-wide)
install:
	go install -ldflags "-X github.com/neuromfs/neuromfs/internal/cli.Version=$(VERSION)" $(CMD_PATH)

## test: Run all tests
test:
	go test ./... -v -count=1

## test-short: Run tests skipping integration tests
test-short:
	go test ./... -short -count=1

## clean: Remove build artefacts
clean:
	rm -rf $(OUT_DIR)
	find . -name '*.neurofs' -prune -o -name 'index.db' -print | xargs rm -f 2>/dev/null || true

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
	gofmt -w .

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## check-retrieval: Retrieval regression gates — fact recall + top-3 precision on both surfaces (thresholds sit under the 2026-07-04 baselines: recall 88.9%, file top-3 66.7%, search top-3 75.0%; bump when an intentional change improves them)
check-retrieval: build
	$(OUT_DIR)/$(BINARY) learn eval --min-recall 0.80
	$(OUT_DIR)/$(BINARY) bench --search --min-top3 60 --min-search-top3 70

## help: Print available targets
help:
	@grep -E '^##' Makefile | sed 's/## //'
