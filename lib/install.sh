#!/bin/sh -
set -euo pipefail

function log {
  echo "[$(date +"%FT%T")] $1"
}

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
GOBIN=$(go env GOBIN)
INSTALL_DIR="${GOBIN:-$(go env GOPATH)/bin}"

log "installing magneto to ${INSTALL_DIR}"
(cd "${REPO_ROOT}" && go install .)

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) log "warning: ${INSTALL_DIR} is not on your PATH" ;;
esac

log "done: $(command -v magneto || echo "${INSTALL_DIR}/magneto")"
