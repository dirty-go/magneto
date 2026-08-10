# magneto

A minimal CLI that downloads media from a magnet link, then exits. Built on
[`anacrolix/torrent`](https://github.com/anacrolix/torrent).

Only files with these extensions are downloaded: `.mp4 .mkv .avi .mp3 .flac
.wav`. If none match, it exits with "No media files found in torrent".

## Install

Requires Go 1.25+.

```sh
go install github.com/dirty-go/magneto@latest
```

Or from a local checkout:

```sh
./lib/install.sh
```

This runs `go install .` and places `magneto` in `$(go env GOBIN)`
(defaults to `$(go env GOPATH)/bin`, typically `~/go/bin`) — make sure
that's on your `PATH`.

## Usage

```sh
magneto -magnet <magnet-uri> [-out <dir>] [-no-seed=<bool>]
```

| Flag       | Default                  | Description                             |
|------------|---------------------------|------------------------------------------|
| `-magnet`  | *(required)*               | Magnet link to download from             |
| `-out`     | `./Downloads`              | Download directory                       |
| `-no-seed` | `true`                     | Disable seeding after download completes |

Running from source with `go run`, `-out` defaults to a `Downloads`
directory next to `main.go`. Running a compiled binary, pass `-out`
explicitly.

Press Ctrl+C to interrupt; progress is saved.

## Docker

Build:

```sh
docker build -t magneto .
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
