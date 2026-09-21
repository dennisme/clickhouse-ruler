# clickhouse-ruler development commands. Run `just` to list recipes.

set shell := ["bash", "-euo", "pipefail", "-c"]

# Matches the address in compose.yaml.
clickhouse_addr := env("RULER_CLICKHOUSE_ADDR", "127.0.0.1:9000")
alertmanager_url := env("RULER_ALERTMANAGER_URL", "http://127.0.0.1:9093")

# List available recipes.
default:
    @just --list

# Install tool versions (mise) and git hooks.
init:
    command -v mise >/dev/null 2>&1 && mise install || true
    if command -v pre-commit >/dev/null 2>&1; then \
        pre-commit install; \
    else \
        echo "pre-commit not found; install via 'pip install pre-commit' or mise"; \
        false; \
    fi

# Build all packages.
build:
    go build ./...

# Unit tests with the race detector. No container needed.
test:
    env -u GOROOT GOTOOLCHAIN=auto go test -race ./...

# Tests against real ClickHouse. Requires the stack: `just compose-up` first.
#
# -p 1 runs one package's tests at a time. More than one integration test
# binds the webhook sink to the fixed port in deploy/alertmanager/alertmanager.yml,
# and two of them running at once would fight over it.
integration:
    RULER_CLICKHOUSE_ADDR="{{clickhouse_addr}}" \
    RULER_ALERTMANAGER_URL="{{alertmanager_url}}" \
        env -u GOROOT GOTOOLCHAIN=auto go test -tags=integration -count=1 -p 1 ./...

# Bring the stack up, run the integration tests, then always tear it down.
integration-clean: compose-up
    #!/usr/bin/env bash
    set -euo pipefail
    trap 'just compose-down' EXIT
    just integration

coverage:
    env -u GOROOT GOTOOLCHAIN=auto go test -count=1 ./... \
        -coverprofile coverage.out -covermode count
    env -u GOROOT GOTOOLCHAIN=auto go tool cover -html=coverage.out -o coverage.html
    env -u GOROOT GOTOOLCHAIN=auto go run github.com/boumenot/gocover-cobertura@v1.4.0 \
        --by-files -ignore-gen-files < coverage.out > coverage.xml

# Run golangci-lint.
lint:
    env -u GOROOT GOTOOLCHAIN=auto golangci-lint run

# Lint markdown with markdownlint-cli2.
markdownlint:
    npx --yes markdownlint-cli2

# Run pre-commit on all files.
pre-commit:
    pre-commit run --all-files

# Start the compose stack and wait for it to be healthy.
compose-up:
    docker compose up -d --wait

# Tear down the stack and delete its volumes, orphans and locally built images.
compose-down:
    # The -v is the part that matters day to day. ClickHouse only runs
    # deploy/clickhouse/init on an empty data directory, so leaving the volume
    # behind means a schema change silently does not apply on the next start.
    #
    # Scoped to this project throughout. No global docker prune, which would
    # take other projects' containers and images with it.
    docker compose down -v --remove-orphans --rmi local

# Everything CI runs, in the order CI runs it.
check: lint test markdownlint
