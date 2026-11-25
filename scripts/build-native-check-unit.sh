#!/usr/bin/env bash
# Verify unit syntax using an existing local binary, without installing or starting it.
set -euo pipefail
repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
command -v systemd-analyze >/dev/null
if [[ ! -x "$repo/bin/tapepilot-native" ]]; then
  printf 'Run make native before checking the service unit.\n' >&2
  exit 1
fi
work=$(mktemp -d /tmp/tapepilot-unit.XXXXXXXX)
trap 'rm -rf -- "$work"' EXIT
# The installation path does not exist during a local source review. Substitute
# only ExecStart's binary path so the verifier can check executability safely.
while IFS= read -r line; do
  if [[ $line == ExecStart=* ]]; then
    printf 'ExecStart=%s -config /etc/tapepilot/config.yaml serve\n' "$repo/bin/tapepilot-native"
  else
    printf '%s\n' "$line"
  fi
done < "$repo/deploy/systemd/tapepilot.service" > "$work/tapepilot.service"
systemd-analyze verify "$work/tapepilot.service"
