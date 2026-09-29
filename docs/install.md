# Install

Three ways to get a ruler: a release archive, the container image, or
`go install`. All three give you the same single binary with the same
subcommands, so pick on how you deploy rather than on what you get.
[Running it](running.md) is what to do once you have one.

Every command here names `v0.1.0`, the current release, so it can be pasted
as written. The [releases page](https://github.com/dennisme/clickhouse-ruler/releases)
is what says whether a newer tag exists; substitute it everywhere below if so.

## A release archive

The shortest path, and the one with nothing between you and the binary. Each
release carries a `tar.gz` per platform, named
`clickhouse-ruler_VERSION_OS_ARCH.tar.gz`, where `VERSION` has no leading `v`
even though the tag does: the archive for the `v0.1.0` tag on 64-bit Linux is
`clickhouse-ruler_0.1.0_Linux_x86_64.tar.gz`. `Linux` and `Darwin` are built,
each as `x86_64` and `arm64`.

```bash
curl -sSLO https://github.com/dennisme/clickhouse-ruler/releases/download/v0.1.0/clickhouse-ruler_0.1.0_Linux_x86_64.tar.gz
curl -sSLO https://github.com/dennisme/clickhouse-ruler/releases/download/v0.1.0/checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

`checksums.txt` lists every archive in the release, so `--ignore-missing` is
what keeps it from failing over the three platforms you did not download. On
macOS the command is `shasum -a 256 --ignore-missing -c checksums.txt`. Verify
before extracting, not after: the point is to know what you are about to run.

The archive holds three files and no directory: `ruler`, `LICENSE` and
`README.md`. It extracts into whatever directory you are standing in.

```bash
tar -xzf clickhouse-ruler_0.1.0_Linux_x86_64.tar.gz
./ruler version
./ruler check --sources rules/sources.yaml rules/
```

There is nothing else to install. The binary carries no runtime dependencies,
reads the files you point it at, and writes to stdout and stderr.

## The container image

Published to GitHub Container Registry for `linux/amd64` and `linux/arm64`, as
the tag itself and as `latest`. Pin the tag in anything you deploy; `latest`
is for trying it out.

```bash
docker pull ghcr.io/dennisme/clickhouse-ruler:v0.1.0
docker run --rm ghcr.io/dennisme/clickhouse-ruler:v0.1.0 version
```

The entrypoint is the binary, so the subcommand and its flags are the
container's arguments. Checking a rules directory means mounting it and naming
the paths inside the container:

```bash
docker run --rm -v "$PWD/rules:/rules:ro" \
  ghcr.io/dennisme/clickhouse-ruler:v0.1.0 \
  check --sources /rules/sources.yaml /rules
```

Running it is the same shape, plus the port it serves on:

```bash
docker run --rm -p 9090:9090 -v "$PWD/rules:/rules:ro" \
  ghcr.io/dennisme/clickhouse-ruler:v0.1.0 \
  run --rules /rules --sources /rules/sources.yaml \
  --alertmanager http://alertmanager:9093
```

One port, `9090`, carrying `/metrics`, `/-/healthy`, `/-/ready`, and
`/-/reload` if you started it with `--enable-reload-endpoint`. It is the
`--listen` address and nothing else listens; change both halves of `-p` if you
move it. Alerts go out over the Alertmanager URL, so that host has to resolve
from inside the container: `localhost` there is the container, not your
machine.

The image runs as an unprivileged user, `ruler`, uid 100. On Linux a bind
mount keeps the host's ownership and permissions, so a rules directory that
only your own user can read is a directory the ruler cannot read. Make the
files readable by others, or pass `--user` with a uid that can read them.
Docker Desktop on macOS maps ownership on the way in and hides this, which is
why a mount that works there can still fail on a Linux host.

Secrets are the same problem from the other side. A source's
`password_file` is a path inside the container, so mount it there and keep it
out of the image.

## `go install`

For a Go toolchain you already have, and the only path that needs one.

```bash
go install github.com/dennisme/clickhouse-ruler/cmd/ruler@v0.1.0
```

The binary lands in `$GOBIN`, or `$(go env GOPATH)/bin` when that is unset,
under the name `ruler`.

This build is not stamped. The release pipeline passes the tag in with
`-ldflags`, and `go install` has no way to, so `ruler version` prints `dev`
with no commit even though the module it built is exactly the tag you asked
for:

```text
ruler dev
commit unknown
built  unknown
dirty  false
go     go1.26.8
```

The tag is still recorded, as the module version rather than as the ruler's
own. `go version -m $(command -v ruler)` prints it on the `mod` line. If which
release a binary came from has to be legible from the binary itself, or from
`clickhouse_ruler_build_info` once it is running, use an archive or the image.

## Which build am I running

`ruler version` answers it on every path, and the answer differs enough to
identify the path:

| Installed from | `version` reports |
| --- | --- |
| Release archive | the tag, the commit it was built from, and the build time |
| Container image | the same facts; the image also carries them as OCI labels |
| `go install` | `dev`, with an unknown commit and build time |

The same version, commit and Go version are labels on
`clickhouse_ruler_build_info`, which is how a half-finished rollout is spotted
from the outside. [Running it](running.md) covers that metric and the rest of
what the ruler exposes.

## The Helm chart

The chart is an OCI artifact beside the image, at the same version:

```sh
helm install ruler oci://ghcr.io/dennisme/charts/clickhouse-ruler \
  --namespace monitoring --values my-values.yaml
```

So `--version 1.2.3` installs the chart that runs the `1.2.3` image, and one
number answers which of either is deployed. It runs the ruler with a git-sync
sidecar by default, which is what delivers the rules and asks the ruler to
re-read them. [How a rule reaches the ruler](deployment.md#how-a-rule-reaches-the-ruler)
covers what the chart decides and what it leaves to you.

## What is not here

No package manager repositories. There is a compose stack in the repository, but
it exists to develop and test against a real ClickHouse rather than to deploy
from; `just compose-up` is documented in
[AGENTS.md](https://github.com/dennisme/clickhouse-ruler/blob/main/AGENTS.md).
For running it for real, [deployment topologies](deployment.md) is the page
that covers the shapes, and each of them is this binary or this image.
