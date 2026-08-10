#!/bin/sh -
set -euo pipefail

function log {
  echo "[$(date +"%FT%T")] $1"
}

usage() {
  echo "Usage: $0 <path-to-download> <magnet-uri>"
  exit 1
}

[ $# -eq 2 ] || usage

DOWNLOAD_PATH=$1
MAGNET_URI=$2

mkdir -p "${DOWNLOAD_PATH}"
DOWNLOAD_PATH=$(cd "${DOWNLOAD_PATH}" && pwd)

IMAGE="${IMAGE:-magneto:latest}"
CONTAINER_DOWNLOAD_DIR="/app/downloads"

log "running ${IMAGE} -> ${DOWNLOAD_PATH}"
docker run --rm \
  -u "$(id -u):$(id -g)" \
  -v "${DOWNLOAD_PATH}:${CONTAINER_DOWNLOAD_DIR}" \
  "${IMAGE}" \
  -magnet "${MAGNET_URI}" \
  -out "${CONTAINER_DOWNLOAD_DIR}"
