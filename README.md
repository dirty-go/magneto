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
magneto -magnet <uri|file>[,<uri|file>...] [-magnet <uri|file>...] [-parallel N] [-out <dir>] [-no-seed=<bool>]
```

| Flag        | Default       | Description                                                        |
|-------------|---------------|--------------------------------------------------------------------|
| `-magnet`   | *(required)*  | Magnet link or batch file; repeat the flag or pass a comma list    |
| `-parallel` | `0`           | Max simultaneous downloads (`0` = all at once)                     |
| `-out`      | `./Downloads` | Download directory                                                 |
| `-no-seed`  | `true`        | Disable seeding after download completes                           |

Multiple magnets download concurrently over a single torrent client:

```sh
magneto -out ~/media -magnet "magnet:?xt=urn:btih:AAA..." -magnet "magnet:?xt=urn:btih:BBB..."
magneto -out ~/media -parallel 2 -magnet "magnet:?xt=...,magnet:?xt=...,magnet:?xt=..."
```

A `-magnet` value starting with `magnet:` (case-insensitive) is a URI; any
other value is read as a batch file with one magnet URI per line. Blank
lines are skipped, surrounding whitespace and CRLF endings are trimmed, and a
leading UTF-8 BOM is ignored. A missing or non-regular file, a line that isn't
a magnet URI (reported as `file:line`), a file with no URIs, or more than
10,000 URIs in one file is a fatal error. Files and URIs can be mixed freely,
and duplicates are dropped:

```sh
magneto -out ~/media -magnet season1.txt -magnet "magnet:?xt=urn:btih:CCC..."
magneto -out ~/media -magnet "season1.txt,season2.txt"
```

Every `-magnet` value is split on commas, so a batch-file path containing a
comma isn't supported; rename the file or pass a path without one.

On a terminal, each torrent gets one line, ordered by its position, that is
redrawn in place every 2 seconds; messages print above the block:

```
[1] Some.Name - 01  42.10% | 295/701 MB | peers: 12
[2] Some.Name - 02 100.00% | done
[3] fetching metadata...
```

If there are more torrents than the terminal has rows, active and failed
lines stay visible and the rest are summarised as `… N more (X done, Y
queued)`. A failed torrent's line shows `failed`. When output is not a
terminal (a pipe, file, `docker logs`, or `TERM=dumb`), each update is
printed as a new plain line instead, with no ANSI escape codes. Control
characters in torrent names and file paths are stripped from all output. A failing magnet is logged and does not
stop the others; the exit code is non-zero if any download failed.

Running from source with `go run`, `-out` defaults to a `Downloads`
directory next to `main.go`. Running a compiled binary, pass `-out`
explicitly.

Press Ctrl+C to interrupt all downloads; progress is saved. Press it again to force-quit.

## Shell completion

Flag completion scripts live in [`completions/`](completions/).

Bash — source it directly, or drop it into your completion directory:

```sh
source completions/magneto.bash
# or: cp completions/magneto.bash /etc/bash_completion.d/magneto
```

Zsh — add `completions/` to your `fpath` before `compinit` runs (in
`~/.zshrc`), or drop `_magneto` into a directory already on `fpath`:

```sh
fpath=(/path/to/magneto/completions $fpath)
autoload -Uz compinit && compinit
# or: cp completions/_magneto "$(brew --prefix)/share/zsh/site-functions/_magneto"
```

Fish — copy it into fish's completions directory:

```sh
cp completions/magneto.fish ~/.config/fish/completions/
```

`-magnet` completes file paths (for batch files) in all three shells.

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
