#!/usr/bin/env bash
# Inventory a local image archive in an isolated scanner. No daemon socket mount.
set -euo pipefail
umask 077
repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
image=${1:?Usage: image-inventory.sh LOCAL_IMAGE [REPORT_DIRECTORY]}
report=${2:-}
if [[ -z $report ]]; then report="$repo/bin/image-inventory"; fi
mkdir -p "$report"
if [[ ! -d $report || -L $report || $(stat -c %a "$report") != 700 || $(stat -c %u "$report") != "$(id -u)" ]]; then
  printf 'Report directory must be owned by this user with mode 0700.\n' >&2
  exit 1
fi
report=$(cd -- "$report" && pwd)
work=$(mktemp -d /tmp/tapepilot-image.XXXXXXXX)
trap 'rm -rf -- "$work"' EXIT
chmod 700 "$work"
mkdir -p "$work/cache/tmp"
scanner=anchore/syft:v1.57.2@sha256:e97e20b6e6d1699c60296b66ab337941724f9bd06ac279d893eda89601fc4b2a
# Pulling this exact scanner is an explicit prerequisite when working offline.
docker image inspect "$scanner" >/dev/null
docker image inspect "$image" > "$report/image-inspect.json"
docker image save --output "$work/image.tar" "$image"
chmod 600 "$work/image.tar"
run=(docker run --rm --network none --read-only --cap-drop ALL --memory 1g --cpus 2 --pids-limit 128
     --security-opt no-new-privileges:true --user "$(id -u):$(id -g)"
     --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777
     -e SYFT_CHECK_FOR_APP_UPDATE=false -e SYFT_PARALLELISM=2
     -e SYFT_GOLANG_CAPTURE_SYMBOLS=all
     -e XDG_CACHE_HOME=/cache -e TMPDIR=/cache/tmp
     --mount "type=bind,src=$work/cache,dst=/cache")
"${run[@]}" --mount "type=bind,src=$work,dst=/input,readonly" "$scanner" scan docker-archive:/input/image.tar -o syft-json > "$report/sbom.syft.json"
chmod 600 "$report/"*.json
"${run[@]}" --mount "type=bind,src=$report,dst=/reports,readonly" "$scanner" convert /reports/sbom.syft.json -o spdx-json > "$report/sbom.spdx.json"
chmod 600 "$report/sbom.spdx.json"
printf 'Local image inventory and SBOM written to %s\n' "$report"
