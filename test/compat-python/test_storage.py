"""Cloud Storage through the official google-cloud-storage client.

Configured by STORAGE_EMULATOR_HOST alone, which the Python client needs with
a scheme: the Go client accepts a bare host, Python does not, and
`cloudburrow env` exports the form both accept.
"""

import io
import os
import urllib.parse

import pytest
from google.api_core import exceptions
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


def test_resumable_upload_resumes_after_interruption(project, suffix):
    """The SDK's own resume path (_media/_upload.py): after a chunk fails,
    recover() asks the session where it stands with Content-Range bytes */*,
    the server answers 308 with the Range it holds, and the upload continues
    from there rather than from the start."""
    from google.cloud.storage._media.requests import ResumableUpload

    client = storage.Client(project=project)
    bucket = client.create_bucket(f"py-resume-{suffix}")
    try:
        chunk = 256 * 1024
        data = os.urandom(3 * chunk + 17)
        url = f"{os.environ['STORAGE_EMULATOR_HOST']}/upload/storage/v1/b/{bucket.name}/o?uploadType=resumable"
        upload = ResumableUpload(url, chunk)
        stream = io.BytesIO(data)
        upload.initiate(client._http, stream, {"name": "resumed.bin"}, "application/octet-stream", total_bytes=len(data))

        resp = upload.transmit_next_chunk(client._http)
        assert resp.status_code == 308
        assert resp.headers["Range"] == f"bytes=0-{chunk - 1}"

        # What a failed request leaves behind: an invalid upload whose stream
        # is past what the server holds.
        stream.seek(len(data))
        upload._make_invalid()
        resp = upload.recover(client._http)
        assert resp.status_code == 308
        assert resp.headers["Range"] == f"bytes=0-{chunk - 1}"
        assert upload.bytes_uploaded == chunk and stream.tell() == chunk

        while not upload.finished:
            upload.transmit_next_chunk(client._http)
        assert bucket.blob("resumed.bin").download_as_bytes() == data
    finally:
        bucket.delete(force=True)


def test_media_link_uses_emulator_host(project, suffix):
    """Blob downloads follow mediaLink (blob.py _get_download_url), so it
    must name the host the client reached, not storage.googleapis.com."""
    client = storage.Client(project=project)
    bucket = client.create_bucket(f"py-link-{suffix}")
    try:
        blob = bucket.blob("linked.txt")
        blob.upload_from_string("linked")
        blob.reload()
        want = urllib.parse.urlsplit(os.environ["STORAGE_EMULATOR_HOST"]).netloc
        assert urllib.parse.urlsplit(blob.media_link).netloc == want
        assert blob.download_as_text() == "linked"
    finally:
        bucket.delete(force=True)


def test_if_generation_match_zero(project, suffix):
    """if_generation_match=0 creates only when no live object exists, and a
    ranged download is inclusive of both ends."""
    client = storage.Client(project=project)
    bucket = client.create_bucket(f"py-gen0-{suffix}")
    try:
        blob = bucket.blob("once.txt")
        blob.upload_from_string("0123456789", if_generation_match=0)
        with pytest.raises(exceptions.PreconditionFailed):
            bucket.blob("once.txt").upload_from_string("again", if_generation_match=0)
        got = bucket.get_blob("once.txt")
        assert got.download_as_bytes(start=2, end=5) == b"2345"
        assert got.download_as_bytes(if_generation_match=got.generation) == b"0123456789"
        with pytest.raises(exceptions.PreconditionFailed):
            got.download_as_bytes(if_generation_match=got.generation + 1)
    finally:
        bucket.delete(force=True)


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
    not os.environ.get("CLOUDBURROW_TEST_SIGNING_KEY"),
    reason="signed URLs are verified against a registered certificate, which "
    "only the storage-server CI step registers (#509, #516)",
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
