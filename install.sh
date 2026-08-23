#!/bin/sh
set -eu

REPO_URL="https://github.com/dirty-go/magneto.git"
WITH_DOCKER=0

log() {
  printf '[%s] %s\n' "$(date +"%FT%T")" "$1"
}

usage() {
  echo "Usage: $0 [--with-docker]"
  exit 1
}

for arg in "$@"; do
  case "$arg" in
    --with-docker) WITH_DOCKER=1 ;;
    -h|--help) usage ;;
    *)
      log "unknown argument: $arg"
      usage
      ;;
  esac
done

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log "error: '$1' is required but not found on PATH"
    exit 1
  fi
}

require git
require go
if [ "$WITH_DOCKER" -eq 1 ]; then
  require docker
fi

TMP_DIR=$(mktemp -d)
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

log "cloning ${REPO_URL} into ${TMP_DIR}"
git clone --depth 1 "$REPO_URL" "$TMP_DIR"

log "installing magneto"
make -C "$TMP_DIR" install

if [ "$WITH_DOCKER" -eq 1 ]; then
  log "building docker image"
  make -C "$TMP_DIR" docker
fi

log "done"
