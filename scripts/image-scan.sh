#!/usr/bin/env bash
# Update only the public vulnerability database, then scan the SBOM offline.
set -euo pipefail
umask 077
repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
report=${1:-}
if [[ -z $report ]]; then report="$repo/bin/image-inventory"; fi
if [[ ! -d $report || -L $report || $(stat -c %a "$report") != 700 || $(stat -c %u "$report") != "$(id -u)" ]]; then
  printf 'Report directory must be owned by this user with mode 0700.\n' >&2
  exit 1
fi
report=$(cd -- "$report" && pwd)
[[ -f "$report/sbom.syft.json" ]] || { printf 'Run image-inventory first.\n' >&2; exit 1; }
scanner=anchore/grype:v0.122.0@sha256:013e16ae96bd64108973e3e9fecec63f7ff276358d34c46c25d33e2aac96539f
docker image inspect "$scanner" >/dev/null
cache=${TAPEPILOT_GRYPE_CACHE_DIR:-$report/grype-db}
mkdir -p "$cache/tmp"
if [[ ! -d $cache || -L $cache || $(stat -c %a "$cache") != 700 || $(stat -c %u "$cache") != "$(id -u)" ]]; then
  printf 'Advisory cache must be owned by this user with mode 0700.\n' >&2
  exit 1
fi
cache=$(cd -- "$cache" && pwd)
common=(docker run --rm --read-only --cap-drop ALL --memory 4g --cpus 2 --pids-limit 128
        --security-opt no-new-privileges:true --user "$(id -u):$(id -g)"
        --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777
        -e GRYPE_CHECK_FOR_APP_UPDATE=false -e GRYPE_DB_AUTO_UPDATE=false
        -e GRYPE_DB_CACHE_DIR=/cache -e XDG_CACHE_HOME=/tmp)
# This networked step sees only its writable advisory cache, with no image,
# source tree, SBOM, catalog, credentials or Docker socket mounted.
"${common[@]}" -e TMPDIR=/cache/tmp --mount "type=bind,src=$cache,dst=/cache" "$scanner" db update
# The scan itself has no network. --fail-on reports actionable high/critical
# findings without treating a nonzero result as a successful security check.
"${common[@]}" --network none --mount "type=bind,src=$cache,dst=/cache,readonly" --mount "type=bind,src=$report,dst=/reports,readonly" "$scanner" sbom:/reports/sbom.syft.json --fail-on high -o json > "$report/vulnerabilities.json"
chmod 600 "$report/vulnerabilities.json"
printf 'Offline vulnerability report written to %s\n' "$report/vulnerabilities.json"
