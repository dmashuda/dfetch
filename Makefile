BINARY_NAME=dfetch
BUILD_DIR=bin
COVERAGE_THRESHOLD=30

# dfetch links against mattn/go-sqlite3 (cgo), so builds require a C compiler.
export CGO_ENABLED=1

# Packages excluding the ANTLR-generated parser (no tests; vet/coverage noise).
GO_PKGS=$(shell go list ./... | grep -v '/internal/sqlparse/gen$$')

# Profiling: `make profile` runs PROFILE_QUERY through the engine and writes CPU +
# memory profiles; `make pprof` opens one in the pprof web UI. Override the query
# with `make profile PROFILE_QUERY="SELECT ..."`, the profile with `make pprof
# PROF=cpu`, and the run count with `BENCHTIME=50x` for a steadier sample.
PROFILE_DIR=prof
PROFILE_QUERY?=SELECT operation_name, duration_ms FROM jaeger.spans WHERE service_name='dfetch' AND start_time >= '2026-06-01T00:00:00Z'
BENCHTIME?=20x
PROF?=mem
PPROF_PORT?=8081

.PHONY: build run test vet lint coverage generate install clean profile pprof

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

profile:
	@mkdir -p $(PROFILE_DIR)
	DFETCH_QUERY="$(PROFILE_QUERY)" go test ./internal/engine \
		-run '^$$' -bench '^BenchmarkProfileQuery$$' -benchmem -benchtime=$(BENCHTIME) \
		-cpuprofile $(PROFILE_DIR)/cpu.prof \
		-memprofile $(PROFILE_DIR)/mem.prof \
		-o $(PROFILE_DIR)/engine.test
	@echo "Wrote $(PROFILE_DIR)/{cpu,mem}.prof — open with: make pprof  (or: make pprof PROF=cpu)"

pprof:
	go tool pprof -http=:$(PPROF_PORT) $(PROFILE_DIR)/engine.test $(PROFILE_DIR)/$(PROF).prof

clean:
	rm -rf $(BUILD_DIR) coverage.out $(PROFILE_DIR)
