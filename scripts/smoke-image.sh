#!/usr/bin/env bash
# Only empty container-local data and public test keys; never mount real auths.
set -Eeuo pipefail
image=${1:?usage: bash scripts/smoke-image.sh IMAGE}
container="ca2a-smoke-${GITHUB_RUN_ID:-$$}-${GITHUB_RUN_ATTEMPT:-1}"
cleanup() {
  local status=$?
  if (( status != 0 )); then
    docker logs "$container" >&2 || true
  fi
  # This exact name belongs to the disposable container created below.
  docker rm --force "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker run --detach --name "$container" \
  --publish 127.0.0.1::7866 \
  --env CA2A_API_KEY=ci-only-api-key \
  --env CA2A_ADMIN_KEY=ci-only-admin-key \
  --env CA2A_LISTEN=:7866 \
  --env CA2A_WATCH_ENABLED=false \
  --env CA2A_CREDITS_AUTO_CLAIM=false \
  "$image" >/dev/null

for ((attempt = 0; attempt < 45; attempt++)); do
  health=$(docker inspect --format '{{.State.Health.Status}}' "$container")
  case "$health" in
    healthy) break ;;
    unhealthy) printf 'Container health check failed\n' >&2; exit 1 ;;
  esac
  sleep 2
done
[[ $health == healthy ]] || { printf 'Timed out waiting for container health\n' >&2; exit 1; }
address=$(docker port "$container" 7866/tcp)
[[ $address == 127.0.0.1:* ]] || { printf 'Unexpected smoke-test port\n' >&2; exit 1; }
[[ $(curl --fail --silent --show-error --max-time 10 "http://$address/healthz") == ok ]]
curl --fail --silent --show-error --max-time 10 "http://$address/panel" >/dev/null
printf 'Image health and embedded panel smoke test passed.\n'
