#!/usr/bin/env bash
set -euo pipefail

mkdir -p dist

targets=(
  "windows amd64 .exe"
  "windows arm64 .exe"
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
)

for target in "${targets[@]}"; do
  read -r os arch ext <<< "${target} "
  ext="${ext// /}"
  output="dist/fetchit-${os}-${arch}${ext}"
  echo "Building ${output}..."
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build -ldflags "-s -w" -o "${output}" .
done

echo "Done! All binaries built in dist/"
