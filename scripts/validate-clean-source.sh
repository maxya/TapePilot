#!/usr/bin/env bash
# Build and test the current local source, including uncommitted implementation.
set -euo pipefail
repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
chmod 700 "$work"
printf 'Clean verification directory: %s\n' "$work"
# Select project inputs explicitly. Host secrets and runtime state stay outside.
cd "$repo"
tar --exclude=node_modules --exclude=.svelte-kit --exclude=web/build \
    -cf - .node-version .dockerignore .github/workflows Makefile README.md NAME_OVERRIDE.md CODEX_BOOTSTRAP.md AGENTS.md SECURITY.md CONTRIBUTING.md \
    go.mod go.sum api cmd compose.yaml config deploy docs internal schemas scripts tests web |
    tar -xf - -C "$work"
cd "$work"
# The source export intentionally has no VCS metadata.
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false"
make install test test-race lint build compose-check compose-readonly-check
