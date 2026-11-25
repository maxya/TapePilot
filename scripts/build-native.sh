#!/usr/bin/env bash
# Assemble a native binary without adding generated assets to the source tree.
set -euo pipefail
repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"
expected_node=$(cat .node-version)
if [[ $(node --version) != "v$expected_node" ]]; then
  printf 'Select Node %s from .node-version before building.\n' "$expected_node" >&2
  exit 1
fi
export GOTOOLCHAIN="go$(awk '$1=="go" {print $2; exit}' go.mod)"
work=$(mktemp -d /tmp/tapepilot-native.XXXXXXXX)
trap 'rm -rf -- "$work"' EXIT
chmod 700 "$work"
npm --prefix web ci
npm --prefix web run build
mkdir -p "$work/src/internal/webui/assets" "$repo/bin"
cp go.mod go.sum "$work/src/"
cp -R cmd "$work/src/"
tar --exclude=./webui/assets -cf - -C internal . | tar -xf - -C "$work/src/internal/"
cp -R web/build/. "$work/src/internal/webui/assets/"
cd "$work/src"
go test -tags embedui ./internal/webui
CGO_ENABLED=0 go build -tags embedui -trimpath -buildvcs=false -o "$repo/bin/tapepilot-native" ./cmd/tapepilot
printf 'Native binary built at %s\n' "$repo/bin/tapepilot-native"
