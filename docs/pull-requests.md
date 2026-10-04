# Checking a pull request

A rule is SQL, and the reason this tool exists is that the SQL is read before
it runs. The place that pays off is the pull request, where a finding is still
one edit away from being fixed.

There is a composite action for it, and everything it does is a flag the binary
already has: the action wires them to GitHub, and nothing in CI is out of reach
on a laptop.

```yaml
name: rules

on: pull_request

permissions:
  contents: read
  pull-requests: write

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          # The merge base is what changed-since compares against, and a
          # shallow checkout has none. The action then checks every rule and
          # says so, which is the safe direction but slower.
          fetch-depth: 0

      - uses: dennisme/clickhouse-ruler/action@v1
        with:
          rules: ./rules
          config: ./rules/ruler.yaml
          changed-since: origin/${{ github.base_ref }}
          comment: true
```

## What the author sees

Two surfaces, and they come from one run of the checks.

**Annotations on the diff.** `format: github` writes
[workflow commands](https://docs.github.com/actions/reference/workflow-commands-for-github-actions)
to stdout and GitHub renders each one on the line it names. No API and no token:

```text
::error file=rules/payments/refunds.yaml,line=11,title=rule/expr::expr does not reference {{ .To }}, so the query has no upper time bound (https://dennisme.github.io/clickhouse-ruler/checks/rule/#rule-expr)
```

GitHub shows `title` as the label on the annotation and the message as its
body, which is why the documentation link sits in the message: the author is
reading the annotation, so that is where somewhere-to-go-next belongs.

**One summary comment**, with `comment: true`, updated in place rather than a
new one per push:

```markdown
1 error, 4 warnings

| Severity | Rule | Check | Finding |
| --- | --- | --- | --- |
| error | [`rules/payments/refunds.yaml:11`](https://github.com/o/r/blob/abc123/rules/payments/refunds.yaml#L11) RefundsAreFailing | [`rule/expr`](checks/rule.md#rule-expr) | expr does not reference {{ .To }}, so the query has no upper time bound |
| warning | [`rules/search/latency.yaml:22`](https://github.com/o/r/blob/abc123/rules/search/latency.yaml#L22) SearchIsSlow | [`annotations/required`](checks/rule.md#annotations-required) | required annotation "runbook_url" is missing |
```

Each row links twice, to the line at the pull request's head commit and to the
check's own page. The table is written by the binary, not assembled in the
action, so it is tested in Go: `ruler check --markdown report.md` produces it
anywhere. Asking for the comment never runs the checks a second time, which
with `online: true` would mean every query twice.

A cost table is a separate thing and needs a cluster. It says what each rule
will read on every evaluation, and [the cost table](running.md#the-cost-table)
is where it lives.

## As a required status

`ruler check` exits non-zero only on an `error`-severity finding, so it gates
exactly the checks your policy sets to `error` and nothing else. The
[exit codes](running.md#what-a-check-exits-with) are worth reading before you
depend on one: a warning exits 0, and so does a cluster the online checks could
not reach.

Three things to require on the repository itself, and two of them are the
mitigation for a failure the checks cannot catch alone:

- **Require branches to be up to date before merging**, or use a merge queue
  once that serialises too much. Two pull requests can each be green against
  the base and not green together, which
  [`rule/duplicate-alert`](checks/rule.md#rule-duplicate-alert),
  [`rule/source-match`](checks/rule.md#rule-source-match) and
  [`ruleset/directory`](checks/policy.md#ruleset-directory) all produce, because
  they are findings about a pair or a tree rather than about a file.
- **Do not filter the workflow by path.** A workflow a `paths` filter skipped
  reports no status at all, and a required status that never reports blocks
  every pull request. In a rules repository there is nothing worth filtering.
- **`CODEOWNERS` on the operator's file.** It carries addresses, credentials and
  caps, and no rule author needs to read it. Not on `policy.yaml`: policy merges
  as a maximum, so a team file cannot loosen what the instance set and a guard
  there would protect nothing.

## Only the rules that changed

`changed-since` takes a git reference, resolves the merge base with `HEAD`, and
keeps the findings that belong to files which differ from it, plus the files git
does not track yet, because a rule written and not yet committed is exactly the
one an author wants checked.

The filter narrows the findings, never the reading. The whole tree is still
loaded, because the source binding and the duplicate checks are answers about a
tree rather than about a file, so the filter can only drop a finding an
unfiltered run would also have reported.

Two cases widen it back to everything, and both say so on stderr:

| What happened | Why everything is checked |
| --- | --- |
| The operator's file or a `policy.yaml` changed | A source's labels or a check's severity moved, so rules in untouched files are affected |
| The base will not resolve: a shallow checkout, or not a repository | Not knowing what changed is a reason to do more work, never less |

It is not a CI-only feature. `ruler check --changed-since origin/main ./rules/`
answers the same question before anything is pushed.

A desk often asks a different question, which is one rule rather than every
rule a branch touched. Paths after the rules directory answer that one:
`ruler check ./rules/ rules/payments/latency.yaml`. Asking for both reports the
changed files among the paths named. See
[Checking one file](running.md#checking-one-file).

## The online checks, and forks

`online: true` adds the checks that need a connection, which read metadata and
no rows. They need network access to the cluster and the credentials in the
operator's file.

A pull request from a fork gets a read-only token and no secrets, so it can do
neither: no online checks, and no comment. The comment step reports that as a
notice and the run carries on. Nothing here uses `pull_request_target` to get
around it, because that runs a fork's code against the base repository with a
write token, which is not a price worth paying for a comment.

## Which version runs

`@v1` is the newest v1 release and picks up fixes without an edit. `@v1.2.3`
pins one release, which is what stops a release that adds a check from turning
every open pull request red.

The action runs the binary from the release its own tag names, so the two
versions match by default. Set `version` to break that on purpose, such as
holding the checker back while a fleet upgrades:

```yaml
      - uses: dennisme/clickhouse-ruler/action@v1
        with:
          version: v1.1.0
          rules: ./rules
```

A branch or a commit is not a release, so `@main` needs `version` set.

Which version you *may* run has a floor. CI and the ruler run the same checks
from the same package, so a checker older than the fleet passes a rule a ruler
then refuses. The floor is the oldest ruler still running, and it is a query
rather than folklore:

```promql
min by (version) (clickhouse_ruler_build_info)
```

## Permissions

| Needed for | Permission |
| --- | --- |
| The offline checks and the annotations | `contents: read` |
| The summary comment as well | `pull-requests: write` |

## Inputs

| Input | Default | What it does |
| --- | --- | --- |
| `rules` | required | The rules directory to check |
| `config` | `ruler.yaml` | The operator's file, which names the sources |
| `policy` | `policy.yaml` beside the rules, if present | A policy file |
| `format` | `github` | `github` for annotations on the diff, or `text`, or `json` |
| `changed-since` | everything | Report only findings in files that differ from the merge base with this reference |
| `online` | `false` | Also run the checks that need a ClickHouse connection |
| `version` | the action's own tag | The release to download and run |
| `binary` | | A prebuilt `ruler` to run instead, which skips the download |
| `comment` | `false` | Post one summary comment, updated in place |
| `github-token` | `${{ github.token }}` | Token for reading and writing that comment |

## Outputs

| Output | What it carries |
| --- | --- |
| `findings` | Path to the JSON findings file, empty unless the run produced one |
| `count` | How many findings were reported, empty unless `format: json` |
| `report` | Path to the markdown findings table, empty unless `comment: true` |

`format: json` is the feed for anything else you want to do with the findings.
Two things about it are contract rather than convenience: a severity is its
name, never a number, and every finding carries its documentation link.

## Without the action

The action is wiring, so there is nothing in it you cannot run yourself. This
is the same check, the same annotations and the same comment body, in a
workflow that downloads nothing:

```yaml
- run: |
    ruler check --config rules/ruler.yaml \
      --format=github \
      --changed-since "origin/$BASE" \
      --markdown report.md \
      --link-prefix "https://github.com/$GITHUB_REPOSITORY/blob/$(git rev-parse HEAD)/" \
      ./rules/
  env:
    BASE: ${{ github.base_ref }}
- run: gh pr comment "$PR" --body-file report.md
  env:
    GH_TOKEN: ${{ github.token }}
    PR: ${{ github.event.pull_request.number }}
```

`--markdown` writes to a path rather than to stdout on purpose. With
`--format=github` stdout carries the workflow commands, and a table in the
middle of them is read as annotations, so `--markdown -` is refused in that
format rather than quietly producing one. `--link-prefix` is the one thing the
binary cannot work out for itself: where the files are served from is a fact
about the host, and a path the repository cannot spell is printed without a
link rather than with a broken one.
