"""Service-account impersonation through google-auth, as docs/credentials.md
describes it: impersonated_credentials with iam_endpoint_override naming the
instance's IAM Credentials endpoint, which the metadata server serves at
GCE_METADATA_HOST.

The source credentials are the generated fixture that default credentials
resolve to, refreshed at its local token_uri. The target is a service
account made through the IAM API (on the Resource Manager port), on which
the fixture's account is granted roles/iam.serviceAccountTokenCreator, as
Google requires (#1747). Every request here is HTTP, so
the socket guard sees each one: a lookup of iamcredentials.googleapis.com,
oauth2.googleapis.com or storage.googleapis.com fails the session.
"""

import datetime

import requests

import google.auth
from google.auth import impersonated_credentials
from google.auth.transport.requests import Request
from google.cloud import storage

from conftest import require


def test_impersonated_token_drives_the_storage_client(project, suffix):
    metadata = require("GCE_METADATA_HOST")
    # The token drives a Storage client; without the instance's storage the
    # client would default to Google, which the loopback guard refuses.
    require("STORAGE_EMULATOR_HOST")
    iam = require("CLOUDBURROW_RESOURCEMANAGER_ENDPOINT")
    if not iam.startswith("http"):
        iam = f"http://{iam}"
    source, _ = google.auth.default(scopes=["https://www.googleapis.com/auth/cloud-platform"])
    # The IAM API calls carry the fixture's token by hand: an
    # AuthorizedSession would look up the account's Regional Access
    # Boundary at iamcredentials.googleapis.com, which the guard refuses.
    source.refresh(Request())
    session = requests.Session()
    session.headers["Authorization"] = f"Bearer {source.token}"
    created = session.post(f"{iam}/v1/projects/{project}/serviceAccounts", json={"accountId": f"py-worker-{suffix}"[:30].rstrip("-")})
    assert created.status_code == 200, created.text
    target = created.json()["email"]
    resource = f"{iam}/v1/projects/{project}/serviceAccounts/{target}"
    granted = session.post(f"{resource}:setIamPolicy", json={"policy": {"bindings": [
        {"role": "roles/iam.serviceAccountTokenCreator", "members": [f"serviceAccount:{source.service_account_email}"]}]}})
    assert granted.status_code == 200, granted.text
    creds = impersonated_credentials.Credentials(
        source_credentials=source,
        target_principal=target,
        target_scopes=["https://www.googleapis.com/auth/devstorage.read_write"],
        iam_endpoint_override=f"http://{metadata}/v1/projects/-/serviceAccounts/{target}:generateAccessToken",
    )

    # After each refresh, google-auth looks up the account's Regional Access
    # Boundary at iamcredentials.googleapis.com, in a background thread, and
    # iam_endpoint_override does not move that lookup (measured with 2.58: the
    # guard caught four attempts). The URL is asserted so that a release that
    # honours the override is noticed, and docs/credentials.md updated. The
    # lookup is then pre-empted with a boundary set up front, through the
    # method google-auth keeps stable for gcloud, so no request leaves loopback.
    assert creds._build_regional_access_boundary_lookup_url().startswith("https://iamcredentials.googleapis.com/")
    creds._set_regional_access_boundary({
        "encodedLocations": "0x0",
        "expiry": datetime.datetime.now(datetime.timezone.utc).replace(tzinfo=None) + datetime.timedelta(hours=6),
    })

    creds.refresh(Request())
    # A local token: nothing here validates it, and no Google service would.
    assert creds.token.startswith("cbl_"), creds.token
    assert creds.expiry is not None

    # The impersonated credentials on the official Storage client. Passed
    # explicitly, they are used instead of the anonymous ones the client picks
    # under STORAGE_EMULATOR_HOST, and every request records its header.
    sent = []
    apply = creds.apply

    def recording_apply(headers, token=None):
        apply(headers, token=token)
        sent.append(headers["authorization"])

    creds.apply = recording_apply
    client = storage.Client(project=project, credentials=creds)
    bucket = client.create_bucket(f"py-imp-{suffix}")
    try:
        bucket.blob("who.txt").upload_from_string("impersonated")
        assert bucket.blob("who.txt").download_as_text() == "impersonated"
        assert bucket.name in [b.name for b in client.list_buckets()]
    finally:
        bucket.delete(force=True)
        session.delete(resource)
    # Every request carried the impersonated token.
    assert sent and all(h == f"Bearer {creds.token}" for h in sent), sent
