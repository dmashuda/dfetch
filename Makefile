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

# Connector query examples live in examples.yaml (single source of truth) and are
# rendered into connectors.md between the <!-- BEGIN/END EXAMPLES --> markers:
#   make examples        regenerate the connectors.md example blocks from examples.yaml
#   make examples-check  fail if connectors.md has drifted (offline)
#   make examples-test   run every example query against the live services
EXAMPLES_YAML?=examples.yaml
EXAMPLES_DOC?=connectors.md

.PHONY: build run test vet lint coverage generate install clean profile pprof \
        examples examples-check examples-test fmt fmt-check gomod2nix gomod2nix-check

# Pin gomod2nix to match the flake input's revision so locally-generated
# lockfiles match what CI / `nix build` expect.
GOMOD2NIX?=go run github.com/nix-community/gomod2nix@latest

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

# Prettier formats the Markdown / YAML / JSON docs and config (Go is handled by
# gofumpt via golangci-lint, not here). Requires Node; the version is pinned in
# package.json / package-lock.json. `make fmt` rewrites files in place;
# `make fmt-check` (run in CI's lint job) fails if anything is unformatted.
node_modules: package.json package-lock.json
	npm ci

fmt: node_modules
	npm exec -- prettier --write .

fmt-check: node_modules
	npm exec -- prettier --check .

# Regenerate the ANTLR SQLite parser. Requires Java (see scripts/gen-parser.sh).
generate:
	./scripts/gen-parser.sh

# Regenerate the Nix per-module lockfile from go.mod/go.sum. Run after any
# dependency change so `nix build` stays in sync; pure Go, no Nix needed.
gomod2nix:
	$(GOMOD2NIX) generate

# Offline CI guard: fail if gomod2nix.toml has drifted from go.mod/go.sum.
gomod2nix-check:
	$(GOMOD2NIX) generate
	@git diff --exit-code --stat gomod2nix.toml \
		|| { echo "FAIL: gomod2nix.toml is stale — run 'make gomod2nix' and commit."; exit 1; }

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

examples:
	go run ./tools/examples -mode gen -yaml $(EXAMPLES_YAML) -readme $(EXAMPLES_DOC)

examples-check:
	go run ./tools/examples -mode check -yaml $(EXAMPLES_YAML) -readme $(EXAMPLES_DOC)

# Runs every example query end-to-end; uses $$GITHUB_TOKEN or `gh auth token` for
# GitHub, and skips the Jaeger group when no local Jaeger is reachable.
examples-test: build
	go run ./tools/examples -mode run -yaml $(EXAMPLES_YAML) -bin ./$(BUILD_DIR)/$(BINARY_NAME)

clean:
	rm -rf $(BUILD_DIR) coverage.out $(PROFILE_DIR)
