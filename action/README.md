# The pull request checker

Validates a repository of ClickHouse alert rules on a pull request: inline
annotations on the diff, and one summary comment updated in place.

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
          # shallow checkout has none.
          fetch-depth: 0

      - uses: dennisme/clickhouse-ruler/action@v1
        with:
          rules: ./rules
          sources: ./rules/ruler.yaml
          changed-since: origin/${{ github.base_ref }}
          comment: true
```

**[Checking a pull request](https://dennisme.github.io/clickhouse-ruler/pull-requests/)**
is the manual: every input and output, the permissions each surface needs, which
version runs, what a required status does and does not gate, and how to run the
same thing without this action. `action.yml` is the wiring itself.

Every check runs in the `ruler` binary, so nothing here is out of reach by hand:

```bash
ruler check --config ruler.yaml --changed-since origin/main ./rules/
```

## What this repository tests

`testdata/` is a rules directory that fails on purpose, and the `action`
workflow runs this action against it three ways: with the checker built from the
checkout, with the summary comment, and with a checksum-verified download of a
published release. Without that, the first run of a release is in somebody
else's repository.
