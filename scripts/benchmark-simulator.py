#!/usr/bin/env python3
"""Three native simulator trials. Reports measures only for this local host."""
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import sqlite3
import subprocess
import tempfile
import time

REPO = Path(__file__).resolve().parents[1]
BINARY = REPO / "bin/tapepilot"
MIB = 1024 * 1024

def timed(argv):
    with tempfile.TemporaryFile() as output:
        start = time.monotonic()
        child = subprocess.Popen(argv, stdout=output, env={**os.environ, "LC_ALL": "C"})
        _, status, usage = os.wait4(child.pid, 0)
        child.returncode = os.waitstatus_to_exitcode(status)
        elapsed = time.monotonic() - start
        if child.returncode:
            raise subprocess.CalledProcessError(child.returncode, argv)
        output.seek(0)
        return dict(wall_seconds=round(elapsed, 6), cpu_seconds=round(usage.ru_utime + usage.ru_stime, 6),
                    peak_rss_kib=usage.ru_maxrss), output.read()

def trial(base, number, source):
    work = base / str(number)
    work.mkdir(mode=0o700)
    state = work / "state"
    config = work / "config.json"
    config.write_text(json.dumps(dict(mode="simulator", state_dir=str(state), sources={"sample": str(source)},
                                      verify_after_write=True, physical_writes_enabled=False)))
    cli = [BINARY, "-config", config]
    def run(*args):
        return subprocess.check_output(cli + list(args))
    run("sim-create", "trial", str(128*MIB))
    run("sim-insert", "trial")
    volume = json.loads(run("initialize", "trial", "Benchmark", "--confirm-synthetic"))["volume_id"]
    run("enroll", volume)
    names = work / "paths.nul"
    names.write_bytes(b"payload.bin\0")
    raw = work / "baseline.tar"
    baseline, _ = timed(["/usr/bin/tar", "--create", "--format=pax", "--blocking-factor=512",
                          "--file", raw, "--directory", source, "--no-recursion", "--null",
                          "--verbatim-files-from", "--files-from", names])
    app, result = timed(cli + ["backup", volume, "sample"])
    job = json.loads(result)
    if job["state"] != "SUCCEEDED":
        raise RuntimeError("Simulation did not produce verified success")
    with sqlite3.connect(f"file:{state}/catalog.db?mode=ro", uri=True) as db:
        events = {c: datetime.fromisoformat(ts) for c, ts in db.execute(
            "SELECT code,ts FROM job_events WHERE job_id=? ORDER BY id", (job["id"],))}
        size, digest = db.execute("SELECT stream_bytes,sha256 FROM archives WHERE job_id=?", (job["id"],)).fetchone()
    tape = state / "simulator/trial/000001.tar"
    with tape.open("rb") as f:
        if hashlib.file_digest(f, "sha256").hexdigest() != digest:
            raise ValueError("Independent stream hash mismatch")
    write = (events["VERIFYING"] - events["RUNNING"]).total_seconds()
    verify = (events["SUCCEEDED"] - events["VERIFYING"]).total_seconds()
    baseline["stream_mib_per_second"] = round(raw.stat().st_size/MIB/baseline["wall_seconds"], 2)
    return dict(trial=number, archive_bytes=size, raw_tar=baseline, complete_backup=app,
                write_and_commit_seconds=write, write_and_commit_mib_per_second=round(size/MIB/write, 2),
                readback_and_index_seconds=verify, readback_and_index_mib_per_second=round(size/MIB/verify, 2))
if __name__ == "__main__":
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="tapepilot-benchmark-") as tmp:
        base = Path(tmp)
        src = base / "source"
        src.mkdir()
        rng = random.Random(109)
        with (src / "payload.bin").open("wb") as f:
            for _ in range(32):
                f.write(rng.randbytes(MIB))
        results = [trial(base, number, src) for number in (1, 2, 3)]
    print(json.dumps(dict(mode="simulator", kernel=platform.release(), architecture=platform.machine(),
                          source_bytes=32*MIB, trials=results), indent=2))
