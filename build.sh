#!/usr/bin/env bash
# build.sh — build myhostmcp: cross-compile the remote half for common targets,
# gzip them into internal/embed/bin/ (so the local binary embeds them), then
# build the local binary.
#
# Usage:
#   ./build.sh                # build everything (local + all remote targets)
#   ./build.sh local          # build only the local binary (uses existing embeds)
#   ./build.sh remote         # build only the remote targets (no local binary)
#   ./build.sh current        # build a remote binary for THIS machine's OS/arch
#                              and place it for the demo / local testing
#
# No SSH credentials or hosts are embedded. The local binary embeds only the
# cross-compiled remote *binaries*, which it uploads to whatever host the agent
# asks it to connect to.

set -euo pipefail
cd "$(dirname "$0")"

EMBED_DIR="internal/embed/bin"
LOCAL_OUT="${MYHOSTMCP_LOCAL_OUT:-build/myhostmcp}"
TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm"
  "darwin/amd64"
  "darwin/arm64"
  "freebsd/amd64"
  "freebsd/arm64"
)

build_remote_targets() {
  mkdir -p "$EMBED_DIR"
  # Remove stale embedded binaries so a target that's no longer in the list
  # doesn't linger and get embedded.
  rm -f "$EMBED_DIR"/*.gz
  # Keep a placeholder so `go build ./...` works before any remote binary exists.
  touch "$EMBED_DIR/.gitkeep"

  for t in "${TARGETS[@]}"; do
    goos="${t%/*}"; goarch="${t#*/}"
    label="${goos}-${goarch}"
    tmp="$(mktemp)"
    echo "→ building remote for $label"
    GOARM=7 CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -tags remote_only -trimpath -ldflags "-s -w" \
      -o "$tmp" ./cmd/myhostmcp
    gzip -9 -c "$tmp" > "$EMBED_DIR/myhostmcp-${label}.gz"
    rm -f "$tmp"
    echo "  $(ls -l "$EMBED_DIR/myhostmcp-${label}.gz" | awk '{print $5 " bytes (gzipped)"}')"
  done
}

build_local() {
  mkdir -p "$(dirname "$LOCAL_OUT")"
  echo "→ building local binary to $LOCAL_OUT"
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$LOCAL_OUT" ./cmd/myhostmcp
  echo "  $(ls -l "$LOCAL_OUT" | awk '{print $5 " bytes"}')"
}

build_current_remote() {
  mkdir -p "$EMBED_DIR"
  label="$(go env GOOS)-$(go env GOARCH)"
  tmp="$(mktemp)"
  echo "→ building remote for current platform ($label)"
  CGO_ENABLED=0 go build -tags remote_only -trimpath -ldflags "-s -w" \
    -o "$tmp" ./cmd/myhostmcp
  gzip -9 -c "$tmp" > "$EMBED_DIR/myhostmcp-${label}.gz"
  rm -f "$tmp"
  echo "  wrote $EMBED_DIR/myhostmcp-${label}.gz"
}

case "${1:-all}" in
  all)
    build_remote_targets
    build_local
    ;;
  local)
    build_local
    ;;
  remote)
    build_remote_targets
    ;;
  current)
    build_current_remote
    ;;
  *)
    echo "usage: $0 [all|local|remote|current]" >&2
    exit 2
    ;;
esac

echo "✓ done"
