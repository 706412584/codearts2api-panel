#!/usr/bin/env bash
# Release versions use a calendar date and a positive daily release number.
set -Eeuo pipefail
version=${1:?usage: bash scripts/check-release-tag.sh vYY.MM.DD.N}
if [[ ! $version =~ ^v([0-9]{2})\.([0-9]{2})\.([0-9]{2})\.([1-9][0-9]*)$ ]]; then
  printf 'Invalid release version: expected vYY.MM.DD.N with N >= 1 and no leading zero.\n' >&2
  exit 1
fi
release_date="20${BASH_REMATCH[1]}-${BASH_REMATCH[2]}-${BASH_REMATCH[3]}"
if [[ $(date --utc --date="$release_date" +%Y-%m-%d 2>/dev/null) != "$release_date" ]]; then
  printf 'Invalid calendar date in release version: %s\n' "$version" >&2
  exit 1
fi
