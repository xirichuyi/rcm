#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p dist internal/assets/agents
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -buildvcs=false -trimpath -tags agentonly -ldflags='-s -w' -o "internal/assets/agents/rcm-linux-$arch" ./cmd/rcm
done
for platform in linux darwin; do
  for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS="$platform" GOARCH="$arch" go build -buildvcs=false -trimpath -ldflags='-s -w' -o "dist/rcm-$platform-$arch" ./cmd/rcm
  done
done
if command -v sha256sum >/dev/null 2>&1; then
  (cd dist && sha256sum rcm-* > SHA256SUMS)
else
  (cd dist && shasum -a 256 rcm-* > SHA256SUMS)
fi
printf 'Release binaries and checksums are in dist/\n'
