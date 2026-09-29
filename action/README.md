# The pull request checker

Validates a repository of ClickHouse alert rules on a pull request: inline
annotations on the diff, and one summary comment updated in place.

Every check runs in the `ruler` binary, so anything this action does in CI is
reachable by hand:

```bash
ruler check --sources sources.yaml --changed-since origin/main ./rules/
```

## Usage

```yaml
name: rules

on: pull_request

permissions:
  contents: read

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          # Deep enough to reach the merge base, which is what changed-since
          # compares against. A shallow checkout has none, so the action checks
          # every rule and says so.
          fetch-depth: 0

      - uses: dennisme/clickhouse-ruler/action@v1
        with:
          rules: ./rules
          sources: ./sources.yaml
          changed-since: origin/${{ github.base_ref }}
```

## Which version runs

A release moves the floating major tag, so `@v1` is the newest v1 release and
gets bug fixes without an edit here. `@v1.2.3` pins one release, which is what
stops a release that adds a check from turning every open pull request red.

Either way the binary is the release the action's tag names. To run a different
one, say an older checker while a fleet is mid-upgrade, name it:

```yaml
      - uses: dennisme/clickhouse-ruler/action@v1
        with:
          version: v1.1.0
          rules: ./rules
```

A branch or a commit is not a release, so `@main` needs `version` set.

## Permissions

| Needed for | Permission |
|---|---|
| The offline checks and the inline annotations | `contents: read` |
| The summary comment | `pull-requests: write` as well |

A pull request from a fork gets neither a writable token nor secrets, so it
cannot run the online checks or post a comment. The comment step reports that as
a notice and the run carries on. Nothing here uses `pull_request_target` to work
around it: that would run a fork's code against the base repository with a write
token, which is not worth a comment.

## Inputs

| Input | Default | What it does |
|---|---|---|
| `rules` | required | The rules directory to check |
| `sources` | `sources.yaml` | The sources file |
| `config` | | A policy file, otherwise `ruler.yaml` beside the rules if present |
| `format` | `github` | `github` for annotations on the diff, or `text`, or `json` |
| `changed-since` | | Report only findings in files that differ from the merge base with this reference |
| `online` | `false` | Also run the checks that need a ClickHouse connection |
| `version` | the tag this action was called with | The release to download |
| `binary` | | A prebuilt `ruler` to run instead, which skips the download |
| `comment` | `false` | Post one summary comment, updated in place |
| `github-token` | `${{ github.token }}` | Token for reading and writing the comment |

## Outputs

| Output | What it carries |
|---|---|
| `findings` | Path to the JSON findings file, empty unless the run produced one |
| `count` | How many findings were reported |

## What this repository tests

`testdata/` is a rules directory that fails on purpose, and the `action`
workflow runs this action against it three ways: with the checker built from the
checkout, with the summary comment, and with a checksum-verified download of a
published release. Without that, the first run of a release is in somebody
else's repository.
