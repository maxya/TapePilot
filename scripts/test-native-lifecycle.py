#!/usr/bin/env python3
"""Exercise the embedded Linux binary with fresh synthetic state and no hardware."""
import http.cookiejar
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

repo = Path(__file__).resolve().parents[1]
binary = repo / "bin/tapepilot-native"
if not binary.is_file():
    raise SystemExit("Build bin/tapepilot-native before running this check.")
if os.geteuid() == 0:
    raise SystemExit("Use an unprivileged user for restore acceptance.")

def exercise(work):
    with socket.socket() as port_probe:
        port_probe.bind(("127.0.0.1", 0))
        origin = f"http://127.0.0.1:{port_probe.getsockname()[1]}"
    env = {**os.environ, "TAPEPILOT_BROWSER_ORIGIN": origin,
           "TAPEPILOT_BROWSER_FIXTURE_DIR": str(work)}
    subprocess.run(["go", "test", "./cmd/tapepilot", "-run", "^TestCreateBrowserFixture$", "-count=1"],
                   cwd=repo, env=env, check=True)
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    def get(path):
        with opener.open(origin + path, timeout=5) as response:
            return response.read()
    for iteration in range(2):
        server = subprocess.Popen([binary, "-config", work / "config.json", "serve"], cwd=repo)
        try:
            for attempt in range(100):
                if server.poll() is not None:
                    raise RuntimeError("Native server exited before startup completed")
                try:
                    if json.loads(get("/api/v1/health"))["status"] == "ok":
                        break
                except (ConnectionError, urllib.error.URLError):
                    time.sleep(0.1)
            else:
                raise RuntimeError("Native health check timed out")
            assert b"TapePilot" in get("/")
            if iteration == 0:
                session = json.loads(get("/api/v1/auth/session"))
                request = urllib.request.Request(origin + "/api/v1/auth/login",
                    data=json.dumps({"password": (work / "password").read_text()}).encode(),
                    headers={"Origin": origin, "Content-Type": "application/json",
                             "X-CSRF-Token": session["csrf_token"]})
                with opener.open(request, timeout=10) as response:
                    assert json.load(response)["authenticated"]
            assert {x["label"] for x in json.loads(get("/api/v1/volumes"))} == {"lab", "other"}
        finally:
            if server.poll() is None:
                server.send_signal(signal.SIGTERM)
            if server.wait(timeout=15) != 0:
                raise RuntimeError("Native service failed to stop cleanly")
with tempfile.TemporaryDirectory(prefix="tapepilot-native-smoke-") as directory:
    exercise(Path(directory))
print("Embedded login, inventory, graceful restart and persisted session passed.")
