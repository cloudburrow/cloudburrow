"""Cloud Storage through the official google-cloud-storage client.

Configured by STORAGE_EMULATOR_HOST alone, which the Python client needs with
a scheme: the Go client accepts a bare host, Python does not, and
`cloudburrow env` exports the form both accept.
"""

import io
import os

import pytest
from google.cloud import storage


def test_bucket_and_object_crud_with_a_resumable_upload(project, suffix):
    client = storage.Client(project=project)
    bucket = client.create_bucket(f"py-{suffix}")
    try:
        assert client.get_bucket(bucket.name).name == bucket.name
        assert bucket.name in [b.name for b in client.list_buckets()]

        small = bucket.blob("small.txt")
        small.upload_from_string("hello from python", content_type="text/plain")
        assert bucket.blob("small.txt").download_as_text() == "hello from python"

        # A chunk size forces the resumable protocol, which a small object
        # would otherwise never use.
        data = b"x" * (512 * 1024 + 123)
        big = bucket.blob("big.bin", chunk_size=256 * 1024)
        big.upload_from_file(io.BytesIO(data), size=len(data))
        assert bucket.blob("big.bin").download_as_bytes() == data

        names = sorted(b.name for b in client.list_blobs(bucket))
        assert names == ["big.bin", "small.txt"]

        small.delete()
        assert not bucket.blob("small.txt").exists()
    finally:
        bucket.delete(force=True)
    assert client.lookup_bucket(bucket.name) is None


@pytest.mark.skipif(
    os.environ.get("CLOUDBURROW_TEST_STORAGE_BACKEND") != "builtin",
    reason="fake-gcs-server's batch answer is not multipart (measured on #534); "
    "this runs against the builtin server (#516)",
)
def test_batch_deletes_and_patches(project, suffix):
    """client.batch() sends one multipart/mixed request (#496): three
    deletes and one patch, each answered in its own part."""
    client = storage.Client(project=project)
    bucket = client.create_bucket(f"py-batch-{suffix}")
    try:
        for name in ("a", "b", "c", "keep"):
            bucket.blob(name).upload_from_string(name)
        keep = bucket.blob("keep")
        with client.batch():
            for name in ("a", "b", "c"):
                bucket.delete_blob(name)
            keep.metadata = {"batched": "yes"}
            keep.patch()
        assert sorted(b.name for b in client.list_blobs(bucket)) == ["keep"]
        assert bucket.get_blob("keep").metadata == {"batched": "yes"}
    finally:
        bucket.delete(force=True)


@pytest.mark.skipif(
    os.environ.get("CLOUDBURROW_TEST_STORAGE_BACKEND") != "builtin",
    reason="fake-gcs-server serves no XML multipart uploads; "
    "this runs against the builtin server (#508, #516)",
)
def test_transfer_manager_xml_multipart_upload(project, suffix, tmp_path):
    """transfer_manager.upload_chunks_concurrently sends a 20 MiB file as an
    XML multipart upload in 5 MiB parts (#508); the object reads back whole,
    with no MD5, as documented."""
    from google.cloud.storage import transfer_manager

    client = storage.Client(project=project)
    bucket = client.create_bucket(f"py-mpu-{suffix}")
    try:
        data = os.urandom(20 << 20)
        path = tmp_path / "big.bin"
        path.write_bytes(data)
        blob = bucket.blob("big.bin")
        transfer_manager.upload_chunks_concurrently(str(path), blob, chunk_size=5 << 20, max_workers=4, worker_type=transfer_manager.THREAD)
        blob.reload()
        assert blob.size == len(data)
        assert blob.md5_hash is None
        assert blob.download_as_bytes() == data
    finally:
        for b in bucket.list_blobs(versions=True):
            b.delete()
        bucket.delete()


@pytest.mark.skipif(
    os.environ.get("CLOUDBURROW_TEST_STORAGE_BACKEND") != "builtin"
    or not os.environ.get("CLOUDBURROW_TEST_SIGNING_KEY"),
    reason="signed URLs are verified by the builtin server started with the "
    "test certificate (#509, #516)",
)
def test_generate_signed_url_v4(project, suffix):
    """Blob.generate_signed_url(version="v4") with a service account key whose
    certificate the server holds reads the object; a tampered one is 403."""
    import datetime
    import urllib.request
    import urllib.error

    from google.oauth2 import service_account

    email = os.environ["CLOUDBURROW_TEST_SIGNING_EMAIL"]
    with open(os.environ["CLOUDBURROW_TEST_SIGNING_KEY"]) as f:
        pem = f.read()
    creds = service_account.Credentials.from_service_account_info(
        {"type": "service_account", "client_email": email, "private_key": pem,
         "token_uri": "https://oauth2.googleapis.com/token", "project_id": project}
    )
    client = storage.Client(project=project)
    bucket = client.create_bucket(f"py-signed-{suffix}")
    try:
        blob = bucket.blob("secret.txt")
        blob.upload_from_string("classified")
        url = blob.generate_signed_url(
            version="v4", expiration=datetime.timedelta(minutes=5), method="GET",
            credentials=creds, api_access_endpoint=os.environ["STORAGE_EMULATOR_HOST"],
        )
        with urllib.request.urlopen(url) as resp:
            assert resp.read() == b"classified"
        with pytest.raises(urllib.error.HTTPError) as err:
            urllib.request.urlopen(url.replace("X-Goog-Signature=", "X-Goog-Signature=00"))
        assert err.value.code == 403
    finally:
        blob.delete()
        bucket.delete()
