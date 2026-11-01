#!/usr/bin/env bash
set -Eeuo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
for version in v26.11.01.1 v26.11.01.12 v00.02.29.1 v24.02.29.3 v26.01.01.1; do
  bash "$root/scripts/check-release-tag.sh" "$version"
done
for version in v26.11.01.0 v26.11.01.01 v26.11.01.-1 v26.11.01 \
  26.11.01.1 v2026.11.01.1 v26.1.01.1 v26.11.1.1 v26.13.01.1 \
  v26.00.01.1 v26.11.00.1 v26.11.31.1 v26.02.29.1 v25.02.29.1 \
  v26.11.01.1-extra 'v26.11.01.1 trailing'; do
  if bash "$root/scripts/check-release-tag.sh" "$version" >/dev/null 2>&1; then
    printf 'Unexpectedly accepted invalid release version: %s\n' "$version" >&2
    exit 1
  fi
done
printf 'Release version checks passed (5 valid and 16 invalid cases).\n'
