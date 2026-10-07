# Releasing

One tag is the whole release. `.github/workflows/goreleaser.yaml` fires on a
`v*` tag and everything below comes out of it; there is no release branch and
no version written in a file.

## Cut one

```sh
just check                  # what CI runs
just release-snapshot       # the same goreleaser path, no tag, writes to dist/
git tag v0.3.0
git push origin v0.3.0
```

`release-snapshot` is the only thing that catches a broken `.goreleaser.yaml`
before a tag exists, and a tag is not something to take back once anybody has
fetched it.

## What the tag publishes

| Artifact | Where |
| --- | --- |
| Archives per OS and architecture, plus `checksums.txt` | the GitHub Release |
| Multi-arch image, tagged with the version and `latest` | `ghcr.io/dennisme/clickhouse-ruler` |
| Helm chart, at the same version as the image | `oci://ghcr.io/dennisme/charts/clickhouse-ruler` |
| The floating major tag, moved to this commit | `v0` for a `v0.x.y` release |

The version comes from the tag on every one of them. `Chart.yaml` carries
`0.0.0` and that is a placeholder which never ships; nothing else in the tree
records a version.

Release notes are GitHub's own generated notes, which is why commit subjects
matter: this repository merges with a merge commit, so a commit log backend
cannot associate a pull request and GitHub's can. `AGENTS.md` has the commit
format.

## The floating major tag

The last step of the workflow force-moves `v0` to the released commit, so
somebody using `dennisme/clickhouse-ruler/action@v0` picks up fixes without
editing their workflow. A prerelease publishes everything else and leaves the
tag alone, because `@v0` should not hand an rc to somebody who did not ask for
one.

That tag is the action's version as well as the tool's. The action resolves a
floating tag to the newest release under that major and prints which one it
picked; `action/README.md` covers pinning and the `version` input that lets a
consumer hold the binary back.

## Prereleases

Tag `v0.3.0-rc1`. Everything publishes, `latest` still moves, and the floating
major tag does not. Nothing else treats a prerelease differently.

## What a release owes the fleet

Validation runs from one package, so the checker in CI and the rulers in the
fleet are the same code only while they are the same version (spec 7.1). Skew
has a safe direction: a checker newer than the fleet blocks a rule the rulers
would have accepted, which is noise, while a checker older than the fleet
passes a rule a ruler then refuses, and a refused reading refuses a start.

So a release that adds or raises a check is one to say so about, and the number
consumers pin has a floor rather than a target:
`min by (version) (clickhouse_ruler_build_info)` over the fleet is the oldest
ruler still running, and the checker has to be at least that new. Spec 10.3 has
the whole argument.
