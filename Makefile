.PHONY: build test clean install run-scan run-ask run-pack run-stats run-bench run-explain deps

BINARY   := neurofs
CMD_PATH := ./cmd/neurofs
OUT_DIR  := ./bin

## deps: Download and tidy Go module dependencies
deps:
	go mod tidy

## build: Compile the neurofs binary to ./bin/neurofs
build:
	@mkdir -p $(OUT_DIR)
	go build -o $(OUT_DIR)/$(BINARY) $(CMD_PATH)
	@echo "built: $(OUT_DIR)/$(BINARY)"

## install: Install neurofs to GOPATH/bin (makes it available system-wide)
install:
	go install $(CMD_PATH)

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

## help: Print available targets
help:
	@grep -E '^##' Makefile | sed 's/## //'
