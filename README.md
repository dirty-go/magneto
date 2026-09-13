# magneto

A minimal CLI that downloads media from one or more magnet links
concurrently, then exits. Built on
[`anacrolix/torrent`](https://github.com/anacrolix/torrent).

Only files with these extensions are downloaded: `.mp4 .mkv .avi .mp3 .flac
.wav`. If a torrent has none, that download fails with "no media files found
in torrent" while the others continue.

## Install

Requires Go 1.25+.

```sh
go install github.com/dirty-go/magneto@latest
```

Or, one-liner that clones the repo and installs it (requires `git` and
`go`; add `--with-docker` to also build the local Docker image, which
additionally requires `docker`):

```sh
curl -fsSL https://raw.githubusercontent.com/dirty-go/magneto/main/install.sh | sh
# or: curl -fsSL .../install.sh | sh -s -- --with-docker
```

Or from a local checkout:

```sh
./lib/install.sh
# or: make install
```

This runs `go install .` and places `magneto` in `$(go env GOBIN)`
(defaults to `$(go env GOPATH)/bin`, typically `~/go/bin`) — make sure
that's on your `PATH`.

## Makefile

- `make build` — `go build` a binary for the host OS/arch into `bin/`
- `make install` — `go install .`
- `make docker` — `docker build`, targeting `--platform linux/<host-arch>`
- `make clean` — remove `bin/`

## Usage

```sh
magneto -magnet <uri>[,<uri>...] [-magnet <uri>...] [-parallel N] [-out <dir>] [-no-seed=<bool>]
```

| Flag        | Default       | Description                                                        |
|-------------|---------------|--------------------------------------------------------------------|
| `-magnet`   | *(required)*  | Magnet link; repeat the flag or pass a comma-separated list        |
| `-parallel` | `0`           | Max simultaneous downloads (`0` = all at once)                     |
| `-out`      | `./Downloads` | Download directory                                                 |
| `-no-seed`  | `true`        | Disable seeding after download completes                           |

Multiple magnets download concurrently over a single torrent client:

```sh
magneto -out ~/media -magnet "magnet:?xt=urn:btih:AAA..." -magnet "magnet:?xt=urn:btih:BBB..."
magneto -out ~/media -parallel 2 -magnet "magnet:?xt=...,magnet:?xt=...,magnet:?xt=..."
```

Each progress line is prefixed with the magnet's position and torrent name
(e.g. `[2] Some.Name  42.10% | 512/1216 MB | peers: 12`). A failing magnet
is logged and does not stop the others; the exit code is non-zero if any
download failed.

Running from source with `go run`, `-out` defaults to a `Downloads`
directory next to `main.go`. Running a compiled binary, pass `-out`
explicitly.

Press Ctrl+C to interrupt all downloads; progress is saved. Press it again to force-quit.

## Docker

Build:

```sh
docker build -t magneto .
# or: make docker
```

Run against a local image:

```sh
./lib/run-docker.sh /path/to/download <magnet-uri>
```

This mounts `/path/to/download` into the container and runs it as your
host UID/GID so the (non-root) container user can write to it. Override
the image with `IMAGE=<name>`.

### Publish to GHCR

Use the `/deploy-ghcr` skill (wraps `lib/ghcr-push.sh`). Requires a
running Docker daemon and a `GITHUB_TOKEN` env var.
