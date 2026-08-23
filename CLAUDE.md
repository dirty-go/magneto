# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`magneto` is a single-file Go CLI (`main.go`) that downloads media from a
magnet link via `github.com/anacrolix/torrent`, then exits (no seeding by
default). There is no `internal/`/`cmd/` structure, no test suite, and no
CI (`.github/` does not exist in this repo at all).

## Commands

- Run: `go run . -magnet <link> [-out <dir>] [-no-seed=<bool>]`
- Build: `go build .`
- Vet: `go vet ./...`
- Format: `gofmt -l .` (auto-applied on edit via a `PostToolUse` hook)
- Lint: `golangci-lint run ./...` (config in `.golangci.yml` — Go's
  `standard` preset plus `unconvert`, `unparam`, `misspell`; `gofmt`/
  `goimports` run as formatters)
- Docker build: `docker build -t magneto .` (multi-stage, non-root final
  image)
- Install locally: `lib/install.sh` (runs `go install .`, checks `PATH`)
- Run the built image: `lib/run-docker.sh <path> <magnet-uri>`
- Push to GHCR: `/deploy-ghcr` skill (wraps `lib/ghcr-push.sh`; requires a
  clean git tree, a running Docker daemon, and `GITHUB_TOKEN`; only run
  when explicitly asked — never automatically)

## Gotchas

- Only downloads files with extensions in a hardcoded allow-list: `.mp4
  .mkv .avi .mp3 .flac .wav`. No matching files → fatal "No media files
  found in torrent".
- `-out` defaults via `runtime.Caller(0)` to a `Downloads` dir next to the
  *source file*. That's meaningless once run from a compiled binary (e.g.
  in the Docker image) — only reliable when run with `go run` from
  source. Pass `-out` explicitly in any other context.
- `go.mod` has no direct `require` block — everything is marked
  `// indirect` even though `anacrolix/torrent` is a direct import. This
  is expected; it still builds fine.
- `-no-seed` defaults to `true`.
- `lib/ghcr-push.sh` has dead `-u -p -n -o -v` flags left over from an
  earlier version — ignore them.
