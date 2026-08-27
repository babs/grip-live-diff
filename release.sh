#!/bin/bash
# A half-built dist must never reach the release upload, so any failing step aborts here.
set -euo pipefail

MODULE=$(grep "^module " go.mod | cut -d" " -f2)
BINBASE=${MODULE##*/}
VERSION=${VERSION:-${GITHUB_REF_NAME:-}}
VERSION=${VERSION:-v0.0.0}
COMMIT_HASH="$(git rev-parse --short HEAD 2>/dev/null || true)"
COMMIT_HASH=${COMMIT_HASH:-00000000}
DIRTY=$(git diff --quiet 2>/dev/null || echo '-dirty')
BUILD_TIMESTAMP=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
BUILDER=$(go version)

rm -rf dist
mkdir dist

# Version vars live in the cmd package, not main
LDFLAGS=(
  "-s -w"
  "-X '${MODULE}/cmd.Version=${VERSION}'"
  "-X '${MODULE}/cmd.CommitHash=${COMMIT_HASH}${DIRTY}'"
  "-X '${MODULE}/cmd.BuildTimestamp=${BUILD_TIMESTAMP}'"
  "-X '${MODULE}/cmd.Builder=${BUILDER}'"
)
echo "[*] Build info"
echo "   Version=${VERSION}"
echo "   CommitHash=${COMMIT_HASH}${DIRTY}"
echo "   BuildTimestamp=${BUILD_TIMESTAMP}"
echo "   Builder=${BUILDER}"

echo "[*] go builds:"
for DIST in {linux,windows}/{amd64,arm64,386} darwin/{amd64,arm64}; do
  GOOS=${DIST%/*}
  GOARCH=${DIST#*/}
  echo "[+]   $DIST:"
  echo "[-]    - build"
  SUFFIX=""
  [ "$GOOS" = "windows" ] && SUFFIX=".exe"
  TARGET=${BINBASE}-${GOOS}-${GOARCH}
  env CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -ldflags="${LDFLAGS[*]}" -o dist/${TARGET}${SUFFIX}
  if [ -z "${NOCOMPRESS:-}" ]; then
    echo "[-]    - compress"
    if [ "$GOOS" = "windows" ]; then
      xz --keep dist/${TARGET}${SUFFIX}
      (cd dist; zip -qm9 ${TARGET}.zip ${TARGET}${SUFFIX})
    else
      xz dist/${TARGET}
    fi
  fi
done

echo "[*] sha256sum"
(cd dist; sha256sum *) | tee ${BINBASE}.sha256sum
mv ${BINBASE}.sha256sum dist/

echo "[*] done"
