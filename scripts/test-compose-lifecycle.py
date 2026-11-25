#!/usr/bin/env python3
"""Run authenticated simulator workflows in a dedicated local Compose project."""
from http.cookiejar import CookieJar
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
from urllib.request import Request, build_opener, HTTPCookieProcessor
from uuid import uuid4

REPO = Path(__file__).resolve().parents[1]

def validate(base):
    with socket.socket() as port_probe:
        port_probe.bind(("127.0.0.1", 0))
        port = port_probe.getsockname()[1]
    origin = f"http://127.0.0.1:{port}"
    env = {**os.environ, "TAPEPILOT_BROWSER_ORIGIN": origin,
           "TAPEPILOT_BROWSER_FIXTURE_DIR": str(base), "TAPEPILOT_PORT": str(port)}
    subprocess.run(["go", "test", "./cmd/tapepilot", "-run", "^TestCreateBrowserFixture$", "-count=1"],
                   cwd=REPO, env=env, check=True)
    # Use a dedicated host-owned private state directory so the non-root test
    # can independently inspect archive bytes and compare restored file content.
    config = json.loads((base / "config.json").read_text())
    config.update(state_dir="/var/lib/tapepilot", sources={"sample": "/sources/sample"},
                  restore_roots={"out": "/restore/out"})
    runtime = base / "container-config.json"
    runtime.write_text(json.dumps(config))
    runtime.chmod(0o600)
    overlay = base / "compose.test.json"
    overlay.write_text(json.dumps({"services": {"backend": {
        "user": f"{os.getuid()}:{os.getgid()}",
        "volumes": [f"{base}/state:/var/lib/tapepilot", f"{base}/source:/sources/sample:ro",
                    f"{base}/restored:/restore/out", f"{runtime}:/etc/tapepilot/config.yaml:ro"]}}}))
    project = "tapepilot-check-" + uuid4().hex[:12]
    def compose(*args, capture=False):
        return subprocess.run(["docker", "compose", "-p", project, "-f", str(REPO / "compose.yaml"),
                               "-f", str(overlay), *args], cwd=REPO, env=env, check=True,
                              text=True, capture_output=capture).stdout

    opener = build_opener(HTTPCookieProcessor(CookieJar()))
    csrf = ""
    def request(route, payload=None, key=None):
        headers = {}
        data = None
        if payload is not None:
            data = json.dumps(payload).encode()
            headers = {"Content-Type": "application/json", "Origin": origin, "X-CSRF-Token": csrf}
            if key:
                headers["Idempotency-Key"] = key
        with opener.open(Request(origin + "/api/v1/" + route, data=data, headers=headers), timeout=15) as response:
            return json.load(response)

    def wait_job(identity):
        for _ in range(240):
            job = request("jobs/" + identity)
            if job["state"] == "SUCCEEDED":
                return
            if job["state"] in ("FAILED", "RECOVERY_REQUIRED", "CANCELLED", "WAITING_MEDIA"):
                raise RuntimeError(f"Unexpected job outcome {job['state']} / {job['error_code']}")
            time.sleep(0.25)
        raise TimeoutError("Simulator job did not complete within 60 seconds")

    try:
        compose("config", "--quiet")
        compose("build")
        # Enrollment is bound to the installation path and profile. Moving the
        # fixture into the container requires explicit local re-enrollment.
        volume = (base / "volume").read_text()
        compose("run", "--rm", "--no-deps", "backend", "-config", "/etc/tapepilot/config.yaml", "enroll", volume)
        compose("up", "-d", "--no-build", "--wait")
        for service in ("backend", "web"):
            ident = compose("ps", "-q", service, capture=True).strip()
            info = json.loads(subprocess.check_output(["docker", "inspect", ident]))[0]
            host = info["HostConfig"]
            assert host["ReadonlyRootfs"] and not host["Privileged"] and not host["Devices"]
            assert "ALL" in host["CapDrop"] and "no-new-privileges:true" in host["SecurityOpt"]
            assert info["Config"]["User"].split(":")[0] not in ("", "0", "root")
            assert all(m["Destination"] != "/dev" for m in info["Mounts"])
            if service == "web":
                assert not any(m["Type"] == "bind" for m in info["Mounts"])
            else:
                assert not next(m["RW"] for m in info["Mounts"] if m["Destination"] == "/sources/sample")
        csrf = request("auth/session")["csrf_token"]
        csrf = request("auth/login", {"password": (base / "password").read_text()})["csrf_token"]
        volume = (base / "volume").read_text()
        profile = request("backup-profiles", {"name": "Compose acceptance", "source_id": "sample",
                          "relative_selection": "", "target_volume_id": volume, "exclusions": [], "enabled": True})
        prefix = "backup-profiles/" + profile["id"] + "/"
        plan = request(prefix + "plan", {"revision": profile["revision"]})
        job = request(prefix + "run", {"revision": profile["revision"], "plan_id": plan["id"]}, str(uuid4()))
        wait_job(job["id"])
        verify = request("volumes/" + volume + "/verify", {}, str(uuid4()))
        wait_job(verify["id"])
        compose("restart", "backend")
        compose("up", "-d", "--wait", "--no-build")
        assert request("auth/session")["authenticated"]
        member = request("catalog/search?q=example.txt")[0]
        selection = request("restores/plan", {"archive_id": member["archive_id"], "member_ids": [member["id"]],
                            "restore_root_id": "out", "destination_slug": "selected"})
        restored = request("restores/run", {"plan_id": selection["id"]}, str(uuid4()))
        wait_job(restored["id"])
        expected = (base / "source/example.txt").read_bytes()
        assert (base / "restored/selected/example.txt").read_bytes() == expected
        assert not (base / "restored/selected/another.txt").exists()
        data = base / "state/simulator/lab/000001.tar"
        assert subprocess.check_output(["tar", "-xOf", data, "example.txt"]) == expected
    finally:
        compose("down")
    print("Compose isolation, verified backup, restart, selective restore and independent GNU tar recovery passed.")

if __name__ == "__main__":
    if os.geteuid() == 0:
        raise SystemExit("Run as an ordinary developer with local Docker access.")
    with tempfile.TemporaryDirectory(prefix="tapepilot-compose-") as folder:
        validate(Path(folder))
