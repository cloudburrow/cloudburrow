"""The guards, proven by running sessions that break them."""

import json
import os
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent


def session(*args, env=None):
    return subprocess.run([sys.executable, "-m", "pytest", "-p", "no:cacheprovider", "-q", *args],
                          cwd=HERE, env=env or os.environ.copy(), capture_output=True, text=True)


def test_a_non_loopback_request_fails_the_session_even_when_caught():
    r = session("-o", "python_files=leak_case.py", "guardcases/leak_case.py")
    assert r.returncode == 4, r.stdout + r.stderr
    assert "192.0.2.1" in r.stdout


def test_a_non_fixture_credentials_file_stops_the_session(tmp_path):
    other = tmp_path / "real-looking.json"
    other.write_text(json.dumps({"type": "service_account", "client_email": "someone@real-project.iam.gserviceaccount.com"}))
    env = os.environ.copy()
    env["GOOGLE_APPLICATION_CREDENTIALS"] = str(other)
    r = session("-o", "python_files=leak_case.py", "guardcases/leak_case.py", env=env)
    assert r.returncode == 3, r.stdout + r.stderr
    assert "not the fixture" in r.stdout + r.stderr
