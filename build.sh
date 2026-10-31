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

# Install locally for current platform if bin directory exists
GOBIN="$(go env GOPATH 2>/dev/null)/bin"
LOCALBIN="${HOME}/.local/bin"

TARGET_BIN=""
case "$(uname -s)" in
  Linux*)
    case "$(uname -m)" in
      x86_64) TARGET_BIN="dist/fetchit-linux-amd64" ;;
      aarch64|arm64) TARGET_BIN="dist/fetchit-linux-arm64" ;;
    esac
    ;;
  Darwin*)
    case "$(uname -m)" in
      arm64) TARGET_BIN="dist/fetchit-darwin-arm64" ;;
      x86_64) TARGET_BIN="dist/fetchit-darwin-amd64" ;;
    esac
    ;;
  MINGW*|MSYS*|CYGWIN*)
    TARGET_BIN="dist/fetchit-windows-amd64.exe"
    ;;
esac

if [ -n "${TARGET_BIN}" ] && [ -f "${TARGET_BIN}" ]; then
  echo "Installing locally to user CLI..."
  if [ -d "${GOBIN}" ]; then
    cp -f "${TARGET_BIN}" "${GOBIN}/fetchit"
    chmod +x "${GOBIN}/fetchit" 2>/dev/null || true
  fi
  if [ -d "${LOCALBIN}" ]; then
    cp -f "${TARGET_BIN}" "${LOCALBIN}/fetchit"
    chmod +x "${LOCALBIN}/fetchit" 2>/dev/null || true
  fi
fi

echo "Done! All binaries built in dist/"
