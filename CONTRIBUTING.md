# Contributing

Two kinds of contribution are useful right now, and the first one matters more
than the second.

The project is early. It takes a rule from a file to a delivered notification
and the chain is proven on a cluster, but no estate outside this repository has
operated it, so what it most needs is evidence from rules somebody else wrote.
Nothing here has external users yet, which means names, flags and metrics can
still change outright.

## Telling us what happened when you ran it

**Checking rules is the safe half.** `ruler check` reads no rows and sends no
notifications, and it reaches your cluster only when you pass `--online`. It
runs on a laptop or in CI against rules you already have:

```bash
ruler check --config ruler.yaml ./rules/
```

If you do that, the single most useful thing you can send back is the findings
feed:

```bash
ruler check --config ruler.yaml --format=json ./rules/ > findings.json
```

Open a **Check findings** issue and attach it. The feed names each check that
fired and the rule it fired on, with no query text, no table names and no rows,
so it carries what we need and nothing about your data.

Why we ask: a check costs a name, a documentation page, a severity and a policy
key forever, so a proposed check is a counting question rather than a design
question (spec 7.8). The clearest open example is the metrics-table mistake in
spec 12.1, which we can reproduce and cannot yet say anybody writes. One real
estate's findings settle that.

**Running it and paging off it is the other half**, and it wants a conversation
first. Open an issue before you point on-call at it so we can tell you what is
soaked and what is not, and so a surprise lands on us rather than on your
responders.

## Reporting a bug

Include the output of `ruler version`, which names the release, the commit and
whether the binary was built from a dirty tree. A report against `main` and a
report against a release are different reports.

Everything the tool knows about a failure is in the log line: the rule, the
source and the file. Passwords never reach a log and driver errors are redacted
before they are returned, so the log line is safe to paste.

## Changing the code

[AGENTS.md](AGENTS.md) is the guide. It has the layout, the conventions, the
command list and the commit format, and it is written for whoever is reading the
code next rather than for a tool. Start there. The short version:

```bash
just init    # mise tool versions and pre-commit hooks
just check   # everything CI runs
just test    # unit tests with -race, no container needed
```

`just check` is the gate. It runs the linter, the build, the race tests, the
generated-documentation diff and markdownlint, in the order CI runs them.

Four things the conventions ask for that are easy to miss:

- **Validation lives in one package.** The same code backs `ruler check` and
  the loader the ruler uses at startup, so a rule that slips past CI still
  cannot run. Do not add a second validation path (spec 7.1).
- **A check is declared once**, in the table in `internal/lint/checks.go`, and
  building a finding goes through `lint.NewProblem`, which refuses a name the
  table does not know. A check therefore cannot ship without a documentation
  page, which a test asserts in both directions (spec 7.8).
- **A flag is registered once**, in `internal/cli`, and the flag table on the
  site is generated from the flag sets. A new flag means `just generate`, which
  `just check` then diffs, so a flag cannot ship undocumented (spec 14).
- **No mocked databases.** Anything that needs ClickHouse is an integration
  test against the real container, with real SQL and real rows. `just
  integration-clean` brings the stack up, runs them and tears it down.

Adding a dependency is a decision to raise rather than one to make. The direct
dependencies are the ClickHouse driver, the Prometheus client and `yaml.v3`.

## Proposing a check

Open a **Check proposal** issue. The design is usually the easy part; what the
issue has to carry is how often the mistake gets written, and by whom. Findings
from your own estate are the strongest form of that, which is why the check
findings issue exists.

A check that has real exceptions ships as a warning rather than an error,
because `error` on a check somebody legitimately needs to break is how a
contributor learns to reach for an exemption (spec 7.6).

## Pull requests

The gate on a rules repository that uses this tool is
`dennisme/clickhouse-ruler/action@v0`. The gate here is `just check` plus the
workflows in `.github/workflows`, one of which runs the action against rules
that fail on purpose.

Commits are Conventional Commits, enforced by a `commit-msg` hook and a CI
check. Subjects end up in the release notes, so write the subject for somebody
reading a changelog. Never bypass a pre-commit hook.

## Licence

Apache-2.0, the same as the rest of the tree. There is no contributor licence
agreement.
