# The Python client of TestEnvResolvesNoGoogleName
# (test/compat/envdns_test.go): google-auth under `cloudburrow env`.
# Application Default Credentials (the fixture GOOGLE_APPLICATION_CREDENTIALS
# names, refreshed at its token_uri) and the metadata server
# (compute_engine, at GCE_METADATA_HOST, pinged at GCE_METADATA_IP). It
# prints "OK python <step>" or "FAIL python <step>: <error>", and runs only
# inside the no-egress container.
import os
import sys
import time

# Never fall back to Google's defaults.
if not os.environ.get("GOOGLE_APPLICATION_CREDENTIALS") or not os.environ.get("GCE_METADATA_HOST"):
    print("FAIL python: GOOGLE_APPLICATION_CREDENTIALS and GCE_METADATA_HOST must be set")
    sys.exit(1)

import google.auth
import google.auth.transport.requests
from google.auth import compute_engine
from google.auth.compute_engine import _metadata

SCOPES = ["https://www.googleapis.com/auth/cloud-platform"]
request = google.auth.transport.requests.Request()


def step(name, f):
    try:
        f()
        print("OK python", name, flush=True)
    except Exception as e:  # noqa: BLE001 - the probe reports every failure
        print("FAIL python", name + ":", repr(e)[:400], flush=True)


def adc():
    creds, project = google.auth.default(scopes=SCOPES)
    if not project:
        raise RuntimeError("no project")
    creds.refresh(request)


def metadata():
    if not _metadata.is_on_gce(request):
        raise RuntimeError("is_on_gce is false")
    _metadata.get_project_id(request)
    compute_engine.Credentials().refresh(request)


step("adc", adc)
step("metadata", metadata)
# Room for anything google-auth starts in the background.
time.sleep(3)
