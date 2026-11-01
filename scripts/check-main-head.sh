#!/usr/bin/env bash
# A manual rerun of an old run must not overwrite prod or deploy an old commit.
set -Eeuo pipefail
: "${GH_TOKEN:?GitHub token is required for the read-only commit check}"
: "${GITHUB_REPOSITORY:?}"
: "${GITHUB_SHA:?}"
latest=$(gh api "repos/$GITHUB_REPOSITORY/git/ref/heads/main" --jq '.object.sha')
if [[ $latest != "$GITHUB_SHA" ]]; then
  printf 'This run is no longer main HEAD; refusing to publish or deploy an older commit.\n' >&2
  printf 'Run the workflow on current main instead. Use the documented server rollback for an intentional rollback.\n' >&2
  exit 1
fi
