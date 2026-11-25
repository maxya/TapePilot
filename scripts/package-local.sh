#!/usr/bin/env bash
# Build a private local review artifact and its inventories. Publishing is a
# separate maintainer action. Inputs are repository source and dependency locks.
set -euo pipefail
umask 077
release_repo=$(cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$release_repo"
release_node=$(cat .node-version)
release_npm=$(node -p 'JSON.parse(require("fs").readFileSync("web/package.json","utf8")).packageManager')
if [[ $(node --version) != "v$release_node" || "npm@$(npm --version)" != "$release_npm" ]]; then
  printf 'Select the pinned Node and npm versions before packaging.\n' >&2
  exit 1
fi
export GOTOOLCHAIN="go$(awk '$1=="go" {print $2; exit}' go.mod)"
export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
release_epoch=${SOURCE_DATE_EPOCH:-0}
if [[ ! $release_epoch =~ ^[0-9]+$ ]]; then
  printf 'SOURCE_DATE_EPOCH must be a nonnegative integer.\n' >&2
  exit 1
fi
export SOURCE_DATE_EPOCH="$release_epoch"
if [[ -L bin || -L bin/release-review ]]; then
  printf 'Review output directories must be ordinary directories.\n' >&2
  exit 1
fi
mkdir -p bin/release-review
release_run=$(mktemp -d "$release_repo/bin/release-review/run.XXXXXXXX")
chmod 700 "$release_run"
release_name=tapepilot-linux-amd64-review
release_package="$release_run/$release_name"
mkdir -p "$release_package/web" "$release_package/licenses/go" "$release_package/licenses/npm" "$release_package/docs" "$release_package/licenses/go-toolchain" "$release_run/web-manifest"
./scripts/build-native.sh
cp bin/tapepilot-native "$release_package/tapepilot"
cp -R web/build/. "$release_package/web/"
cp config/tapepilot.example.yaml "$release_package/tapepilot.example.yaml"
cp SECURITY.md CONTRIBUTING.md AGENTS.md CODEX_BOOTSTRAP.md NAME_OVERRIDE.md "$release_package/"
cp README.md "$release_package/README-original.md"
cp -R docs/. "$release_package/docs/"
cp docs/release/README.md "$release_package/README.md"
release_goroot=$(go env GOROOT)
cp "$release_goroot/LICENSE" "$release_package/licenses/go-toolchain/"
if [[ -f $release_goroot/PATENTS ]]; then cp "$release_goroot/PATENTS" "$release_package/licenses/go-toolchain/"; fi
go version -m -json "$release_package/tapepilot" > "$release_run/buildinfo.raw.json"
go list -m -json all > "$release_run/modules.raw.json"
node --version > "$release_run/node-version"
npm --version > "$release_run/npm-version"
python3 - "$release_repo" "$release_run" "$release_package" "$release_epoch" <<'PY'
import datetime
import hashlib
import json
import pathlib
import shutil
import sys
import urllib.parse

repo, run, package = [pathlib.Path(value) for value in sys.argv[1:4]]
epoch = int(sys.argv[4])
created = datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')

def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')

def digest(path):
    result = hashlib.sha256()
    with path.open('rb') as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b''):
            result.update(chunk)
    return result.hexdigest()

def license_files(root, destination):
    found = []
    if not root.is_dir():
        return found
    resolved = root.resolve()
    for candidate in sorted(root.iterdir()):
        if not candidate.name.upper().startswith(('LICENSE', 'COPYING', 'NOTICE', 'UNLICENSE')):
            continue
        if not candidate.is_file() or not candidate.resolve().is_relative_to(resolved):
            continue
        destination.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(candidate, destination / candidate.name)
        found.append(str((destination / candidate.name).relative_to(package)))
    return found

raw = (run / 'modules.raw.json').read_text()
decoder = json.JSONDecoder()
modules = {}
while raw.strip():
    value, length = decoder.raw_decode(raw.lstrip())
    modules[value['Path']] = value
    raw = raw.lstrip()[length:]
info = json.loads((run / 'buildinfo.raw.json').read_text())
write_json(package / 'binary-buildinfo.json', info)
node_version = (run / 'node-version').read_text().strip()
npm_version = (run / 'npm-version').read_text().strip()
status = {
    'artifact_purpose': 'local maintainer review',
    'target': 'linux/amd64',
    'project_license': 'pending maintainer selection',
    'physical_hardware_qualification': 'pending',
    'production_release_approved': False,
    'image_advisory_review': 'check final image reports in docs/VALIDATION_REPORT.md before distribution',
    'go': info['GoVersion'],
    'node': node_version,
    'npm': npm_version,
    'source_date_epoch': epoch,
}
write_json(package / 'review-status.json', status)
(package / 'PROJECT-LICENSE-PENDING.txt').write_text(
    'TapePilot project licensing awaits maintainer approval.\n'
    'This artifact is for local review. Distribution approval remains pending.\n'
    'Third-party license texts and declarations are included under licenses/.\n'
)
inventory = {'native_modules': [], 'web_build_packages': [], 'license_review_required': []}
packages = [{
    'SPDXID': 'SPDXRef-TapePilot', 'name': 'TapePilot Linux amd64 native review binary',
    'versionInfo': 'local-review', 'downloadLocation': 'NOASSERTION',
    'filesAnalyzed': False, 'licenseConcluded': 'NOASSERTION',
    'licenseDeclared': 'NOASSERTION', 'copyrightText': 'NOASSERTION',
    'checksums': [{'algorithm': 'SHA256', 'checksumValue': digest(package / 'tapepilot')}],
}]
packages.append({
    'SPDXID': 'SPDXRef-Go-Stdlib', 'name': 'stdlib', 'versionInfo': info['GoVersion'],
    'downloadLocation': 'NOASSERTION', 'filesAnalyzed': False,
    'licenseConcluded': 'NOASSERTION', 'licenseDeclared': 'BSD-3-Clause',
    'copyrightText': 'NOASSERTION',
    'externalRefs': [{'referenceCategory': 'PACKAGE-MANAGER', 'referenceType': 'purl', 'referenceLocator': 'pkg:golang/stdlib@' + info['GoVersion']}],
})
relationships = [
    {'spdxElementId': 'SPDXRef-DOCUMENT', 'relationshipType': 'DESCRIBES', 'relatedSpdxElement': 'SPDXRef-TapePilot'},
    {'spdxElementId': 'SPDXRef-TapePilot', 'relationshipType': 'DEPENDS_ON', 'relatedSpdxElement': 'SPDXRef-Go-Stdlib'},
]
for dependency in info.get('Deps', []):
    name, version = dependency['Path'], dependency['Version']
    metadata = modules.get(name, {})
    label = name.replace('/', '__') + '@' + version
    directory = metadata.get('Dir')
    notices = license_files(pathlib.Path(directory), package / 'licenses/go' / label) if directory else []
    row = {'module': name, 'version': version, 'sum': dependency.get('Sum', ''), 'license_files': notices}
    inventory['native_modules'].append(row)
    identifier = 'SPDXRef-Go-' + hashlib.sha256((name + '@' + version).encode()).hexdigest()[:20]
    packages.append({
        'SPDXID': identifier, 'name': name, 'versionInfo': version,
        'downloadLocation': 'NOASSERTION', 'filesAnalyzed': False,
        'licenseConcluded': 'NOASSERTION', 'licenseDeclared': 'NOASSERTION',
        'copyrightText': 'NOASSERTION',
        'externalRefs': [{'referenceCategory': 'PACKAGE-MANAGER', 'referenceType': 'purl', 'referenceLocator': 'pkg:golang/' + urllib.parse.quote(name, safe='/') + '@' + version}],
    })
    relationships.append({'spdxElementId': 'SPDXRef-TapePilot', 'relationshipType': 'DEPENDS_ON', 'relatedSpdxElement': identifier})
lock = json.loads((repo / 'web/package-lock.json').read_text())
curated_notices = json.loads((repo / 'docs/release/third-party-notices.json').read_text())
for key, metadata in sorted(lock['packages'].items()):
    if not key:
        continue
    directory = repo / 'web' / key
    name = metadata.get('name') or key.split('node_modules/')[-1]
    label = name.replace('/', '__') + '@' + metadata.get('version', 'unknown')
    destination = package / 'licenses/npm' / label
    notices = license_files(directory, destination)
    notice_origin = 'installed package files'
    complete = bool(notices)
    curated = curated_notices.get(name + '@' + metadata.get('version', ''))
    if directory.is_dir() and not notices and curated:
        source = repo / 'docs/release' / curated['file']
        destination.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, destination / 'LICENSE-upstream')
        notices = [str((destination / 'LICENSE-upstream').relative_to(package))]
        notice_origin = curated['source']
        complete = True
    if directory.is_dir() and not notices and name.startswith('@rolldown/binding-'):
        parent = repo / 'web/node_modules/rolldown'
        parent_metadata = json.loads((parent / 'package.json').read_text())
        if parent_metadata['version'] == metadata.get('version'):
            notices = license_files(parent, destination)
            complete = bool(notices)
            notice_origin = 'matching-version rolldown package notice'
    if directory.is_dir() and not notices:
        destination.mkdir(parents=True, exist_ok=True)
        installed_metadata = json.loads((directory / 'package.json').read_text())
        declaration = {'name': name, 'version': metadata.get('version'), 'license': metadata.get('license', 'NOASSERTION'), 'author': installed_metadata.get('author'), 'repository': installed_metadata.get('repository'), 'status': 'Full upstream license text requires maintainer confirmation before redistribution.'}
        write_json(destination / 'LICENSE-DECLARATION.json', declaration)
        notices = [str((destination / 'LICENSE-DECLARATION.json').relative_to(package))]
        for filename in ['README.md', 'readme.md']:
            readme = directory / filename
            if readme.is_file() and readme.resolve().is_relative_to(directory.resolve()):
                shutil.copyfile(readme, destination / filename)
                notices.append(str((destination / filename).relative_to(package)))
                break
        inventory['license_review_required'].append(declaration)
    inventory['web_build_packages'].append({
        'name': name, 'version': metadata.get('version'),
        'declared_license': metadata.get('license', 'NOASSERTION'),
        'integrity': metadata.get('integrity'), 'installed': directory.is_dir(),
        'license_files': notices, 'license_text_complete': complete, 'notice_origin': notice_origin,
    })
write_json(package / 'dependency-inventory.json', inventory)
write_json(package / 'sbom-native.spdx.json', {
    'spdxVersion': 'SPDX-2.3', 'dataLicense': 'CC0-1.0', 'SPDXID': 'SPDXRef-DOCUMENT',
    'name': 'TapePilot local native review inventory',
    'documentNamespace': 'https://spdx.org/spdxdocs/tapepilot-local-' + digest(package / 'tapepilot'),
    'creationInfo': {'creators': ['Tool: TapePilot package-local'], 'created': created},
    'packages': packages, 'relationships': relationships,
})
# npm needs a versioned root to emit a PURL. Supply a review-only version in
# disposable metadata while preserving the repository's exact dependency lock.
manifest = json.loads((repo / 'web/package.json').read_text())
manifest['version'] = '0.0.0-review'
lock['version'] = '0.0.0-review'
lock['packages']['']['version'] = '0.0.0-review'
write_json(run / 'web-manifest/package.json', manifest)
write_json(run / 'web-manifest/package-lock.json', lock)
missing = [item['module'] for item in inventory['native_modules'] if not item['license_files']]
if missing:
    raise SystemExit('Missing native dependency license files: ' + ', '.join(missing))
PY
npm --prefix "$release_run/web-manifest" sbom --sbom-format=cyclonedx --package-lock-only > "$release_package/sbom-web-build.cyclonedx.json"
python3 - "$release_package" "$release_epoch" <<'PY'
import hashlib
import json
import pathlib
import sys
import datetime
import uuid
root = pathlib.Path(sys.argv[1])
epoch = int(sys.argv[2])
web_bom = json.loads((root / 'sbom-web-build.cyclonedx.json').read_text())
web_bom['metadata']['timestamp'] = datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
web_bom['metadata']['component']['name'] = 'tapepilot-web'
web_bom.pop('serialNumber', None)
identity = hashlib.sha256(json.dumps(web_bom, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
web_bom['serialNumber'] = 'urn:uuid:' + str(uuid.uuid5(uuid.NAMESPACE_URL, 'tapepilot-web-build:' + identity))
(root / 'sbom-web-build.cyclonedx.json').write_text(json.dumps(web_bom, indent=2, sort_keys=True) + '\n')
files = []
for path in sorted(root.rglob('*')):
    if path.is_file():
        files.append({'path': str(path.relative_to(root)), 'bytes': path.stat().st_size, 'sha256': hashlib.sha256(path.read_bytes()).hexdigest()})
(root / 'file-inventory.json').write_text(json.dumps(files, indent=2) + '\n')
PY
(
  cd "$release_package"
  find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
  sha256sum --check SHA256SUMS > "$release_run/checksum-validation.log"
)
tar --sort=name --mtime="@$release_epoch" --owner=0 --group=0 --numeric-owner -cf - -C "$release_run" "$release_name" | gzip -n > "$release_run/$release_name.tar.gz"
(
  cd "$release_run"
  sha256sum "$release_name.tar.gz" > "$release_name.tar.gz.sha256"
  sha256sum --check "$release_name.tar.gz.sha256" >> checksum-validation.log
)
printf 'Local review artifact: %s\n' "$release_run/$release_name.tar.gz"
printf 'Checksums verified; project license, physical qualification and release approval remain pending.\n'
