# AGENTS.md

Guidance for coding agents working in this repository.

## What this is

`clickhouse-ruler` is a single Go service that evaluates Prometheus-style alert
rules written as ClickHouse SQL and sends the results to an Alertmanager you
already run. Rules and sources are YAML files in git. The distinguishing
feature is that the SQL itself is checked before it runs, not stored as an
opaque string.

Read [README.md](README.md) for the user-facing picture and
[spec.md](spec.md) (which indexes `spec/`) for the design. The spec is the
source of truth for intended behaviour. If code and spec disagree, say so
rather than picking one silently.

## Commands

Everything goes through [`just`](https://github.com/casey/just). Run `just` on
its own to list recipes.

```bash
just check              # what CI runs: lint, build, unit tests, markdownlint
just test               # unit tests with -race, no container needed
just lint               # golangci-lint, formatting checked via `fmt --diff`
just fix                # apply every fix golangci-lint can make
just build              # go build ./... plus a vet of the integration-tagged tests
just integration-clean  # start the stack, run integration tests, tear it down
just markdownlint       # markdownlint-cli2 over the docs
just init               # mise tool versions and pre-commit hooks
```

Run `just check` before claiming work is done. If you touched Markdown, that
includes `just markdownlint`, which CI runs too.

Running the binary directly:

```bash
go run ./cmd/ruler check --sources rules/sources.yaml rules/
go run ./cmd/ruler check --online --sources rules/sources.yaml rules/
go run ./cmd/ruler run --rules ./rules --sources ./rules/sources.yaml \
  --alertmanager http://localhost:9093
```

### Integration tests

Behind the `integration` build tag, so `just test` never runs them. They need
the compose stack:

```bash
just compose-up
just integration
just compose-down
```

Two things that bite:

- `just integration` uses `-p 1`. Several integration tests bind a webhook
  sink to the fixed port in `deploy/alertmanager/alertmanager.yml`, so parallel
  packages fight over it. Do not remove that flag.
- `compose-down` passes `-v` on purpose. ClickHouse only applies
  `deploy/clickhouse/init` to an empty data directory, so keeping the volume
  means a schema change silently does not take effect.

## Layout

| Path | What lives there |
| --- | --- |
| `cmd/ruler` | CLI: `check`, `run`, `inspect`, `privileges` subcommands and flag parsing |
| `internal/rule` | Rule file parsing and the offline rule checks |
| `internal/source` | Sources file parsing, secret loading, label matching, the ClickHouse user contract in `privileges` |
| `internal/ruleset` | Loading a rules directory and binding each rule to the sources its selector matches |
| `internal/policy` | Check severity configuration and policy merging |
| `internal/lint` | The shared vocabulary: the check table in `checks.go`, severities, findings, and output formats (text, GitHub workflow commands) |
| `internal/query` | Query execution, SQL AST checks, driver-error redaction |
| `internal/alert` | Alert state machine: pending, firing, resolved, `for`, identity, fingerprints |
| `internal/scheduler` | Group ticking, concurrency limits, metrics, HTTP surface, shutdown |
| `internal/notify` | Alertmanager payloads, resend cadence, the HTTP client |
| `deploy/` | ClickHouse init SQL (including the reference ruler user) and Alertmanager config |
| `spec/` | Design, validation, operations, research, decisions |

## Conventions

- Go 1.26. Module `github.com/dennisme/clickhouse-ruler`. Standard library
  first; the only direct dependencies are the ClickHouse driver, the Prometheus
  client and `yaml.v3`. Adding a dependency is a decision to raise, not to make.
- `golangci-lint` v2 with `misspell`, `unconvert`, `unparam`, `gosec`, plus
  `gofmt` and `goimports` as formatters. `gosec` is excluded in `_test.go`
  because fixtures and fake credentials trip G304 and G101.
- Comments here explain why a thing is the way it is, often at length, and the
  `justfile` does the same. Match that. Never write a comment about what the
  code used to do or that something changed.
- Validation lives in one package with two entry points: the same code backs
  `ruler check` and the loader used at startup, so a rule that slips past CI
  still cannot run. Do not add a second validation path. See spec 7.1.
- Findings carry a file, a line and a severity. Correctness checks always
  block; convention checks take their severity from policy.
- Every check is declared once, in the table in `internal/lint/checks.go`,
  with its default severity, key list and the spec section behind it. Building
  a finding goes through `lint.NewProblem`, which refuses a name the table
  does not know, so a check cannot ship without an entry. See spec 7.8.
- Metrics are labelled by rule and group, never by alert instance. A rule
  returning 10,000 rows still produces one series. See spec 8.3.
- Passwords never reach a log. ClickHouse driver errors go through
  `internal/query/redact.go` before being returned.

## Testing

- Tests are table-driven with fixtures in each package's `testdata/`. Add a
  fixture rather than embedding a large YAML string in the test.
- No mocked databases. Anything that needs ClickHouse is an integration test
  against the real container, with real SQL and real rows.
- Integration tests need both the `//go:build integration` tag and the matching
  `//go:build` comment placement; `just build` vets them so a signature change
  fails now instead of hours later.
- Test output must be clean. If a test expects an error, capture and assert the
  error output rather than letting it print.

## Commits

Conventional Commits, enforced by commitizen (`.cz.toml`) via a `commit-msg`
hook and a CI check. Allowed types: `feat`, `fix`, `doc`, `perf`, `ref`,
`test`, `chore`, `ci`, `revert`. Format:

```text
<type>(<optional scope>): <subject>
```

Never bypass a pre-commit hook. Do not commit unless asked to.

## Status

The project is early and has no external users, so names, flags and metrics can
be changed outright without migration paths or compatibility shims. The README
`Status` section lists what works and what is missing; keep it accurate when
you land something it describes.
