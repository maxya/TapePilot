#!/usr/bin/env python3
"""Recover two independently committed synthetic backups on stock Linux."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

REPO = Path(__file__).resolve().parents[1]
IMAGE = "node:24.22.1-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205"

def check(work):
    source = work / "source"
    media = work / "recovery-input"
    source.mkdir()
    media.mkdir()
    conf = work / "config.json"
    conf.write_text(json.dumps(dict(mode="simulator", state_dir=str(work / "state"),
                                   physical_writes_enabled=False, verify_after_write=True,
                                   sources={"example": str(source)})))
    def tp(*args):
        return subprocess.check_output([REPO / "bin/tapepilot", "-config", conf, *args])
    tp("sim-create", "recovery", str(32 << 20))
    tp("sim-insert", "recovery")
    volume = json.loads(tp("initialize", "recovery", "Recovery acceptance", "--confirm-synthetic"))["volume_id"]
    tp("enroll", volume)
    expected = []
    for seq in (1, 2):
        data = f"Independently recoverable fixture, generation {seq}.\n".encode()
        (source / "sample.txt").write_bytes(data)
        expected.append(hashlib.sha256(data).hexdigest())
        result = json.loads(tp("backup", volume, "example"))
        assert result["state"] == "SUCCEEDED"
    for index in range(5):
        filename = f"{index:06d}.tar"
        shutil.copyfile(work / "state" / "simulator" / "recovery" / filename, media / filename)
    # The recovery host receives only five ordinary TAR files and two independently
    # retained source hashes. No project filesystem, catalog or binary is mounted.
    verifier = r"""
const {execFileSync: run} = require('node:child_process');
const fs = require('node:fs');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const [volume, ...expected] = process.argv.slice(2);
const file = i => '/media/' + String(i).padStart(6,'0') + '.tar';
const sha = path => run('sha256sum',['--',path],{encoding:'utf8'}).split(' ')[0];
const member = (i, name) => run('tar',['--extract','--to-stdout','--file',file(i),'--',name]);
const header = JSON.parse(member(0,'tapepilot/volume.json'));
assert.equal(header.magic,'tapepilot-volume');
assert.equal(header.volume_id,volume);
const label = sha(file(0));
let previous = null;
for (let seq=1;seq<=2;seq++){
  const data=2*seq-1, ci=data+1, commit=JSON.parse(member(ci,'tapepilot/commit.json'));
  assert.equal(commit.magic,'tapepilot-commit');
  assert.equal(commit.volume_id,volume);
  assert.equal(commit.sequence,seq);
  assert.equal(commit.volume_header_sha256,label);
  assert.equal(commit.previous_commit_sha256??null,previous);
  assert.equal(commit.data_file_index,data);
  assert.equal(commit.commit_file_index,ci);
  assert.equal(commit.data_stream_bytes,fs.statSync(file(data)).size);
  assert.equal(commit.data_record_count*262144,commit.data_stream_bytes);
  assert.equal(commit.data_sha256,sha(file(data)));
  const restored=member(data,'sample.txt');
  assert.equal(crypto.createHash('sha256').update(restored).digest('hex'),expected[seq-1]);
  previous=sha(file(ci));
}
console.log('Two complete pairs recovered with GNU tar and checked against original content hashes.');
"""
    subprocess.run(["docker", "run", "--rm", "-i", "--network", "none", "--read-only", "--cap-drop", "ALL",
                    "--security-opt", "no-new-privileges", "--user", f"{os.getuid()}:{os.getgid()}",
                    "--mount", f"type=bind,src={media},dst=/media,readonly",
                    IMAGE, "node", "-", volume, *expected], input=verifier, text=True, check=True)
if __name__ == "__main__":
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="tapepilot-independent-") as base:
        check(Path(base))
