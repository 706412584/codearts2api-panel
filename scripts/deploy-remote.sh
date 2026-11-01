#!/usr/bin/env bash
# Runs on the GitHub-hosted runner; never copy production data to the runner.
set -Eeuo pipefail
umask 077

fail() { printf 'remote deploy: %s\n' "$*" >&2; exit 1; }
: "${DEPLOY_HOST:?Set DEPLOY_HOST}"
: "${DEPLOY_USER:?Set DEPLOY_USER}"
: "${DEPLOY_SSH_KEY:?Set the DEPLOY_SSH_KEY secret}"
: "${DEPLOY_KNOWN_HOSTS:?Set the verified DEPLOY_KNOWN_HOSTS secret}"
: "${IMAGE_REF:?Set the published image digest}"
: "${GITHUB_RUN_ID:?}"
: "${GITHUB_RUN_ATTEMPT:?}"
DEPLOY_PORT=${DEPLOY_PORT:-22}
DEPLOY_PATH=${DEPLOY_PATH:-/opt/codearts2api-panel}

# These values become part of the remote shell command; allow no shell syntax,
# traversal, whitespace, IPv6 literals, or option-like hosts/users/paths.
[[ $DEPLOY_HOST =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]] || fail 'DEPLOY_HOST must be a DNS name or IPv4 address'
[[ $DEPLOY_USER =~ ^[a-z_][a-z0-9_-]*$ ]] || fail 'invalid DEPLOY_USER'
[[ $DEPLOY_PORT =~ ^[0-9]{1,5}$ ]] || fail 'invalid DEPLOY_PORT'
(( 10#$DEPLOY_PORT >= 1 && 10#$DEPLOY_PORT <= 65535 )) || fail 'invalid DEPLOY_PORT'
[[ $DEPLOY_PATH =~ ^(/[a-zA-Z0-9_-][a-zA-Z0-9_.-]*)+$ && $DEPLOY_PATH != *..* ]] || fail 'DEPLOY_PATH must be an absolute path without spaces or dot-dot'
[[ $GITHUB_RUN_ID =~ ^[0-9]+$ && $GITHUB_RUN_ATTEMPT =~ ^[0-9]+$ ]] || fail 'invalid run identifier'
[[ $IMAGE_REF =~ ^ghcr\.io/[a-z0-9]+([._-][a-z0-9]+)*/[a-z0-9]+([._-][a-z0-9]+)*@sha256:[a-f0-9]{64}$ ]] || fail 'IMAGE_REF must be an immutable GHCR digest'

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
ssh_dir=$(mktemp -d "${RUNNER_TEMP:-/tmp}/ca2a-ssh.XXXXXX")
cleanup() {
  if [[ -n ${ssh_dir:-} && $ssh_dir == "${RUNNER_TEMP:-/tmp}"/ca2a-ssh.* && -d $ssh_dir ]]; then
    rm -rf -- "$ssh_dir"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf '%s\n' "$DEPLOY_SSH_KEY" > "$ssh_dir/key"
printf '%s\n' "$DEPLOY_KNOWN_HOSTS" > "$ssh_dir/known_hosts"
unset DEPLOY_SSH_KEY DEPLOY_KNOWN_HOSTS
chmod 600 "$ssh_dir/key" "$ssh_dir/known_hosts"
ssh_options=(
  -i "$ssh_dir/key" -o IdentitiesOnly=yes -o BatchMode=yes
  -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=$ssh_dir/known_hosts"
  -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=15
  -o ServerAliveInterval=15 -o ServerAliveCountMax=4
)
target="$DEPLOY_USER@$DEPLOY_HOST"
release_dir="$DEPLOY_PATH/releases/$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
ssh "${ssh_options[@]}" -p "$DEPLOY_PORT" "$target" \
  "umask 077; test -d '$DEPLOY_PATH' && mkdir -p -- '$release_dir'"
scp "${ssh_options[@]}" -P "$DEPLOY_PORT" \
  "$root/deploy/deploy.sh" "$root/deploy/compose.yml" "$target:$release_dir/"
# The server is pre-authenticated to private GHCR packages if necessary. The
# runner's GITHUB_TOKEN is intentionally NOT sent to or persisted on the server.
ssh "${ssh_options[@]}" -p "$DEPLOY_PORT" "$target" \
  "bash '$release_dir/deploy.sh' '$DEPLOY_PATH' '$IMAGE_REF' '$release_dir/compose.yml'"
