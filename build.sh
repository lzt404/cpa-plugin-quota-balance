#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

ID="quota-balance"
VERSION="1.0.0"
GOOS_TARGET="$(go env GOOS)"
GOARCH_TARGET="$(go env GOARCH)"
EXT="so"
[[ "$GOOS_TARGET" == darwin ]] && EXT="dylib"

mkdir -p .build dist

# Optional: build against an unreleased core checkout through a temporary go.work,
# so go.mod keeps pointing at the published module that CI resolves.
if [[ -n "${CPA_CORE:-}" ]]; then
  work="$PWD/.build/go.work"
  version="$(awk '$1 == "go" {print $2; exit}' "$CPA_CORE/go.mod")"
  printf 'go %s\nuse (\n %s\n %s\n)\n' "$version" "$PWD" "$(realpath "$CPA_CORE")" > "$work"
  export GOWORK="$work"
else
  unset GOWORK || true
fi

go test -race -v ./...

stage="dist/${VERSION}-${GOOS_TARGET}-${GOARCH_TARGET}"
rm -rf "$stage"
mkdir -p "$stage"
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o "$stage/${ID}.${EXT}" .
rm -f "$stage/${ID}.h"
cp -f manifest.json README.md README.zh-CN.md LICENSE "$stage/"
cp -f "$(go env GOROOT)/LICENSE" "$stage/GO-LICENSE"
yaml="$(go list -m -f '{{.Dir}}' gopkg.in/yaml.v3)"
cp -f "$yaml/LICENSE" "$stage/YAML-LICENSE"
[[ ! -f "$yaml/NOTICE" ]] || cp -f "$yaml/NOTICE" "$stage/YAML-NOTICE"
( cd "$stage" && sha256sum ./* > SHA256SUMS )

# The store validates the archive, not the release: the dynamic library must sit at
# the zip root under <id>.so even when the host build produces a .dylib locally.
native_lib="$stage/${ID}.${EXT}"
if [[ "$EXT" != so ]]; then
  cp -f "$native_lib" "$stage/${ID}.so"
fi
python3 package.py "$stage" "dist/${ID}_${VERSION}_${GOOS_TARGET}_${GOARCH_TARGET}.zip"
if [[ "$EXT" != so ]]; then
  rm -f "$stage/${ID}.so"
fi

mkdir -p dist/artifacts
cp -f "dist/${ID}_${VERSION}_${GOOS_TARGET}_${GOARCH_TARGET}.zip" dist/artifacts/
( cd dist/artifacts && sha256sum ./*.zip > checksums.txt && cat checksums.txt )