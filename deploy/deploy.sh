#!/usr/bin/env bash
# Deploy one GHCR image without replacing configuration or persistent data.
set -Eeuo pipefail
umask 077

fail() { printf 'deploy: %s\n' "$*" >&2; exit 1; }

if (( $# < 2 || $# > 3 )); then
  fail 'usage: bash deploy.sh DEPLOY_DIR GHCR_IMAGE [COMPOSE_FILE]'
fi

[[ -d $1 ]] || fail 'DEPLOY_DIR must already exist; follow docs/deployment.md first'
DEPLOY_DIR=$(cd -- "$1" && pwd -P)
[[ $DEPLOY_DIR != / ]] || fail 'refusing to use the filesystem root as DEPLOY_DIR'
requested_image=$2
compose_file=${3:-$(dirname -- "${BASH_SOURCE[0]}")/compose.yml}
image_repository='ghcr\.io/[a-z0-9]+([._-][a-z0-9]+)*/[a-z0-9]+([._-][a-z0-9]+)*'
tagged_image="^${image_repository}:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$"
digest_image="^${image_repository}@sha256:[a-f0-9]{64}$"
[[ $requested_image =~ $tagged_image || $requested_image =~ $digest_image ]] || fail 'expected a GHCR owner/image:tag or owner/image@sha256:digest'
[[ -f $compose_file ]] || fail 'COMPOSE_FILE must be a regular file'
[[ -f $DEPLOY_DIR/config.json ]] || fail 'config.json must exist as a regular file (not a directory)'
[[ -d $DEPLOY_DIR/auths && -d $DEPLOY_DIR/data ]] || fail 'auths/ and data/ must already exist'
command -v docker >/dev/null || fail 'docker is required'
command -v flock >/dev/null || fail 'flock is required (Debian: util-linux)'
docker compose version >/dev/null || fail 'Docker Compose V2 or newer is required'

# Server-side lock also protects manual deployments and separate workflow runs.
exec 9>"$DEPLOY_DIR/.deploy.lock"
flock -n 9 || fail 'another deployment is in progress'
stage=$(mktemp -d "$DEPLOY_DIR/.deploy.XXXXXX")
cleanup() {
  # Verify the absolute target before removing our own temporary directory.
  if [[ -n ${stage:-} && $stage == "$DEPLOY_DIR"/.deploy.* && -d $stage ]]; then
    rm -rf -- "$stage"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Snapshot before modifying state: COMPOSE_FILE may be .previous-compose.yml.
cp -- "$compose_file" "$stage/candidate.compose.yml"

export DEPLOY_DIR
export IMAGE_REF=$requested_image
compose() {
  local file=$1
  shift
  docker compose --project-name codearts2api-panel \
    --project-directory "$DEPLOY_DIR" --env-file /dev/null --file "$file" "$@"
}
candidate() { compose "$stage/candidate.compose.yml" "$@"; }
previous() { compose "$stage/previous.compose.yml" "$@"; }
start_args=(up --detach --no-build --pull never --wait --wait-timeout 120 codearts2api-panel)
candidate config --quiet

# Use the actual container's pinned image, not a moving tag or stale state file.
old_image=''
container_id=$(candidate ps --all --quiet codearts2api-panel)
if [[ -n $container_id ]]; then
  [[ $container_id != *$'\n'* ]] || fail 'expected a single managed container'
  [[ -f $DEPLOY_DIR/.current-compose.yml ]] || fail 'existing container is not managed by deploy.sh; migrate it manually first'
  old_image=$(docker inspect --format '{{.Config.Image}}' "$container_id")
  [[ $old_image =~ $digest_image ]] || fail 'existing container has no pinned GHCR image; migrate it manually first'
  cp -- "$DEPLOY_DIR/.current-compose.yml" "$stage/previous.compose.yml"
  IMAGE_REF=$old_image previous config --quiet
fi

docker pull "$requested_image"
# A tag can have digests under several repositories; select the requested one.
repository=${requested_image%%@*}
repository=${repository%%:*}
repo_digests=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$requested_image")
resolved_image=''
while IFS= read -r ref; do
  if [[ $ref == "$repository"@sha256:* && $ref =~ $digest_image ]]; then
    if [[ $requested_image =~ $digest_image ]]; then
      # The same image ID can belong to multiple manifest lists. Match exactly,
      # rather than rejecting a valid release because an older digest is first.
      [[ $ref != "$requested_image" ]] || resolved_image=$requested_image
    else
      [[ -z $resolved_image || $resolved_image == "$ref" ]] || fail 'tag has multiple local digests; deploy the exact digest from the workflow summary instead'
      resolved_image=$ref
    fi
  fi
done <<< "$repo_digests"
[[ -n $resolved_image ]] || fail 'could not resolve the pulled image to the requested GHCR digest'
export IMAGE_REF=$resolved_image
printf 'Deploying %s\n' "$IMAGE_REF"

if ! candidate "${start_args[@]}"; then
  if [[ -n $old_image ]]; then
    printf 'Health check failed; restoring %s\n' "$old_image" >&2
    if IMAGE_REF=$old_image previous "${start_args[@]}"; then
      printf 'Previous image restored. Deployment is still marked failed.\n' >&2
    else
      printf 'ROLLBACK FAILED: inspect the container on the server immediately.\n' >&2
    fi
  else
    printf 'First deployment failed; no previous version exists. Removing only the failed service container.\n' >&2
    if candidate stop codearts2api-panel; then
      # No --volumes: bind-mounted configuration and data remain untouched.
      candidate rm --force codearts2api-panel || printf 'Could not remove the failed container; inspect it before retrying.\n' >&2
    else
      printf 'Could not stop the failed container; inspect it before retrying.\n' >&2
    fi
  fi
  exit 1
fi

# Save rollback metadata only after the candidate becomes healthy. Sensitive
# config/auths/data are never copied, overwritten, deleted or sent to the runner.
if [[ -n $old_image ]]; then
  printf '%s\n' "$old_image" > "$stage/previous-image"
  mv -f -- "$stage/previous-image" "$DEPLOY_DIR/.previous-image"
  mv -f -- "$stage/previous.compose.yml" "$DEPLOY_DIR/.previous-compose.yml"
fi
printf '%s\n' "$IMAGE_REF" > "$stage/current-image"
mv -f -- "$stage/current-image" "$DEPLOY_DIR/.current-image"
mv -f -- "$stage/candidate.compose.yml" "$DEPLOY_DIR/.current-compose.yml"
printf 'Healthy: %s\n' "$IMAGE_REF"
