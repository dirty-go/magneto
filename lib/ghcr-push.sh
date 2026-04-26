#!/bin/sh -
set -euo pipefail

function log {
  echo "[$(date +"%FT%T")] $1"
}

check_docker() {
  # Candidate socket paths
  local SOCKETS=(
    "$HOME/.orbstack/run/docker.sock"
    "$HOME/.docker/run/docker.sock"
    "/var/run/docker.sock"
  )

  # Try each known socket location
  for s in "${SOCKETS[@]}"; do
    if [ -S "$s" ]; then
      if curl -s --unix-socket "$s" http/_ping >/dev/null 2>&1; then
        log "docker daemon already running via socket: $s"
        return 0
      fi
    fi
  done

  log "make sure docker daemon is running"
  exit 1
}

if [[ $(git diff --stat) != '' ]]; then
    log "Working directory is dirty. Please commit the changes before continuing."
    exit 1
fi

check_docker
log "Continuing with build or deployment..."

NODE_VERSION="16"
VARIANT="2-1.24-trixie"
while getopts u:p:n:o:v: flag
do
  case "${flag}" in
    u) # Enter gitlab user-id for generating .netrc inside docker, used for gitlab private repo
       USERID=${OPTARG};;
    p) # Enter gitlab personal access token
       PAT=${OPTARG};;
    n) # Enter node version
      NODE_VERSION=${OPTARG};;
    v) # golang variant
      VARIANT=${OPTARG};;
    o) # Path for docker saved, this will be used for docker save location
        dir=${OPTARG}
        if [ -d $dir ]; then
          SAVE=1
          outdir=${dir}
        fi
        ;;
  esac
done

OWNER="${OWNER:-iamucil}"
GHCR_HOST="ghcr.io"
REPOSITORY="${REPOSITORY:-magneto}"
RESOLVED_RESOURCE_VERSION=$(git rev-parse HEAD)

# docker login into github repository
# Check if GITHUB_TOKEN is set
if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  echo "❌ GITHUB_TOKEN environment variable is not set."
  echo "Please export it first, e.g.:"
  echo "  export GITHUB_TOKEN=ghp_yourtokenhere"
  exit 1
fi

# Check if GITHUB_TOKEN is in the correct format
if [[ ! "${GITHUB_TOKEN}" =~ ^ghp_[a-zA-Z0-9_]{36,}$ ]]; then echo "Invalid GITHUB_TOKEN format"; exit 1; fi

# Log in to GitHub Container Registry using the token
echo "🔐 Logging in to GitHub Container Registry..."
TMP_FILE=$(mktemp)
echo "${GITHUB_TOKEN}" > "$TMP_FILE"
docker login ghcr.io -u "${GITHUB_USERNAME:-$OWNER}" --password-stdin < "$TMP_FILE"
rm "$TMP_FILE"

echo "✅ Successfully logged in to ghcr.io as ${GITHUB_USERNAME:-$OWNER}"

IMAGE_REPO_NAME=${REPOSITORY}
IMAGE_REPO_URL=${GHCR_HOST}/${OWNER}/${REPOSITORY}
IMAGE_TAG=$RESOLVED_RESOURCE_VERSION
docker buildx build \
  -f Dockerfile \
  -t ${IMAGE_REPO_NAME}:${IMAGE_TAG} \
  .

docker tag ${IMAGE_REPO_NAME}:${IMAGE_TAG}  ${IMAGE_REPO_URL}:${IMAGE_TAG}
for i in {1..3}; do docker push ${IMAGE_REPO_URL}:${IMAGE_TAG} && break || sleep 5; done

docker tag ${IMAGE_REPO_NAME}:${IMAGE_TAG}  ${IMAGE_REPO_URL}:latest
docker push  ${IMAGE_REPO_URL}:latest

if docker push ${IMAGE_REPO_URL}:${IMAGE_TAG}; then
  docker rmi ${IMAGE_REPO_NAME}:${IMAGE_TAG}
else
  echo "Push failed"
  exit 1
fi

log "✅ Successfully build ${IMAGE_REPO_URL}:${IMAGE_TAG}"
