BINARY_NAME=dfetch
BUILD_DIR=bin
COVERAGE_THRESHOLD=30

# dfetch links against mattn/go-sqlite3 (cgo), so builds require a C compiler.
export CGO_ENABLED=1

# Packages excluding the ANTLR-generated parser (no tests; vet/coverage noise).
GO_PKGS=$(shell go list ./... | grep -v '/internal/sqlparse/gen$$')

.PHONY: build run test vet lint coverage generate install clean

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) .

run:
	go run . $(ARGS)

test:
	go test ./...

vet:
	# -unreachable=false: vet analyzes the imported ANTLR-generated parser, which
	# has unavoidable unreachable-code artifacts. golangci-lint still runs govet
	# (incl. unreachable) on our own code, with the gen/ dir excluded by path.
	go vet -unreachable=false $(GO_PKGS)

lint:
	golangci-lint run ./...

# Regenerate the ANTLR SQLite parser. Requires Java (see scripts/gen-parser.sh).
generate:
	./scripts/gen-parser.sh

coverage:
	@go test -coverprofile=coverage.out $(GO_PKGS)
	@COVERAGE=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}' | tr -d '%'); \
	echo "Total coverage: $${COVERAGE}%"; \
	if [ "$$(echo "$${COVERAGE} < $(COVERAGE_THRESHOLD)" | bc)" -eq 1 ]; then \
		echo "FAIL: Coverage $${COVERAGE}% is below the $(COVERAGE_THRESHOLD)% threshold"; \
		rm -f coverage.out; \
		exit 1; \
	fi; \
	rm -f coverage.out

install:
	go install .

clean:
	rm -rf $(BUILD_DIR) coverage.out
