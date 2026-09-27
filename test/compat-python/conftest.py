"""Official Python SDK compatibility suite for CloudBurrow (#287).

Configuration comes from the running instance itself: `cloudburrow env
--format json` for every variable a developer would export, and nothing
else. Two guards hold for the whole run:

* ADC: GOOGLE_APPLICATION_CREDENTIALS must be the fixture `cloudburrow env`
  generated, and google.auth.default() must resolve to it. Otherwise a
  misconfigured endpoint could succeed against real Google.
* Loopback: every connection made through Python sockets must be to a
  loopback address. That covers the HTTP clients, such as Cloud Storage. The
  gRPC clients connect from C, where Python cannot see, so for those the
  guard checks every channel target instead: channel_target() refuses
  anything that is not loopback, and each gRPC test builds its channel through
  it. The emulator variables the clients build their own channels from
  (GRPC_EMULATOR_VARIABLES) are checked the same way.

A violation fails the session even if the test that caused it caught the
exception.
"""

import ipaddress
import json
import os
import shlex
import socket
import subprocess

import pytest

VIOLATIONS = []

# Read by the official clients themselves, each of which then dials a
# plaintext gRPC channel no Python socket sees. Checked at session start.
GRPC_EMULATOR_VARIABLES = (
    "PUBSUB_EMULATOR_HOST",
    "FIRESTORE_EMULATOR_HOST",
    "DATASTORE_EMULATOR_HOST",
    "BIGTABLE_EMULATOR_HOST",
    "SPANNER_EMULATOR_HOST",
)


class GuardError(RuntimeError):
    """A request would have left loopback, or real credentials were in use."""


def is_loopback(host):
    host = host.strip("[]")
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def refuse(what):
    VIOLATIONS.append(what)
    raise GuardError(what)


def channel_target(addr):
    """Return addr for a gRPC channel, refusing any non-loopback target."""
    host = addr.rsplit(":", 1)[0]
    if not is_loopback(host):
        refuse(f"gRPC channel target {addr!r} is not loopback")
    return addr


_real_connect = socket.socket.connect
_real_getaddrinfo = socket.getaddrinfo


def _guarded_connect(self, address):
    if self.family in (socket.AF_INET, socket.AF_INET6) and not is_loopback(str(address[0])):
        refuse(f"connection to {address!r} is not loopback")
    return _real_connect(self, address)


def _guarded_getaddrinfo(host, *args, **kwargs):
    # Refused before any lookup: resolving www.googleapis.com is already a
    # request that left the machine.
    if host is not None and not is_loopback(str(host)):
        refuse(f"name lookup for {host!r} is not loopback")
    return _real_getaddrinfo(host, *args, **kwargs)


def load_instance_env():
    """The instance's variables, from `cloudburrow env --format json`, or
    from CLOUDBURROW_ENV_JSON, a file in that same format, for a server run
    without an instance (the builtin `cloudburrow storage-server` in CI)."""
    if os.environ.get("CLOUDBURROW_ENV_JSON"):
        with open(os.environ["CLOUDBURROW_ENV_JSON"]) as f:
            return json.load(f)
    binary = os.environ.get("CLOUDBURROW_BIN", "cloudburrow")
    args = shlex.split(os.environ.get("CLOUDBURROW_ARGS", ""))
    out = subprocess.run([binary, "env", "--format", "json", *args], check=True, capture_output=True, text=True).stdout
    return json.loads(out)


def pytest_sessionstart(session):
    try:
        env = load_instance_env()
    except (OSError, subprocess.CalledProcessError) as err:
        pytest.exit(f"cannot read the instance's environment from `cloudburrow env`: {err}", returncode=2)

    fixture = env.get("GOOGLE_APPLICATION_CREDENTIALS", "")
    # Empty is unset: CI clears it deliberately. Anything else must be the
    # fixture itself.
    given = os.environ.get("GOOGLE_APPLICATION_CREDENTIALS", "")
    if given and given != fixture:
        pytest.exit(
            f"GOOGLE_APPLICATION_CREDENTIALS is {given!r}, not the fixture {fixture!r}: "
            "refusing to run where real credentials could be used",
            returncode=3,
        )
    for key, value in env.items():
        os.environ[key] = value
    # The default-credentials lookup must land on the fixture and nowhere
    # else, such as a gcloud login in the home directory.
    import google.auth

    creds, _ = google.auth.default()
    with open(fixture) as f:
        email = json.load(f).get("client_email")
    if getattr(creds, "service_account_email", None) != email:
        pytest.exit(f"application default credentials resolved to {creds!r}, not the fixture", returncode=3)

    for name in GRPC_EMULATOR_VARIABLES:
        if name in os.environ:
            channel_target(os.environ[name])
    socket.socket.connect = _guarded_connect
    socket.getaddrinfo = _guarded_getaddrinfo


def pytest_sessionfinish(session, exitstatus):
    if VIOLATIONS:
        print("\nloopback or credentials guard violations:\n  " + "\n  ".join(VIOLATIONS))
        session.exitstatus = 4


@pytest.fixture
def project():
    return os.environ["GOOGLE_CLOUD_PROJECT"]


@pytest.fixture
def suffix(request):
    """A per-test suffix, so reruns against one instance never collide."""
    import time

    return f"{os.getpid()}-{int(time.time() * 1000) % 10**9}"


def require(name):
    """The value of an instance variable, or a skip when the instance does not
    export it: the opt-in services are tested only where they are enabled,
    as the Go suite's harness does."""
    value = os.environ.get(name, "")
    if not value:
        pytest.skip(f"{name} is not exported by this instance: its service is not enabled")
    return value
