#!/usr/bin/env bash
# Cross-compile self-contained release archives; no credentials or real data.
set -Eeuo pipefail
version=${1:?usage: bash scripts/release.sh VERSION NEW_OUTPUT_DIRECTORY}
output=${2:?usage: bash scripts/release.sh VERSION NEW_OUTPUT_DIRECTORY}
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
bash "$root/scripts/check-release-tag.sh" "$version"
# Refuse an existing directory so old assets cannot leak into a new release.
mkdir -- "$output"
output=$(cd -- "$output" && pwd -P)
stage=$(mktemp -d "$output/.build.XXXXXX")
cleanup() {
  if [[ $stage == "$output"/.build.* && -d $stage ]]; then
    rm -rf -- "$stage"
  fi
}
trap cleanup EXIT
cd -- "$root"

for target in windows/amd64 linux/amd64 linux/arm64; do
  goos=${target%/*}
  goarch=${target#*/}
  name="codearts2api-panel_${version}_${goos}_${goarch}"
  package_dir="$stage/$name"
  mkdir -- "$package_dir"
  binary=codearts2api-panel
  if [[ $goos == windows ]]; then binary+=.exe; fi
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath \
    -ldflags="-s -w -X codearts2api-panel/internal/server.Version=$version" \
    -o "$package_dir/$binary" ./cmd/server
  cp -- config.example.json README.md LICENSE THIRD_PARTY_NOTICES.md "$package_dir/"
  if [[ $goos == windows ]]; then
    (cd -- "$stage" && zip --quiet --recurse-paths "$output/$name.zip" "$name")
  else
    tar --create --gzip --file "$output/$name.tar.gz" --directory "$stage" "$name"
  fi
done
(cd -- "$output" && sha256sum -- *.zip *.tar.gz > SHA256SUMS)
printf 'Release archives and checksums: %s\n' "$output"
