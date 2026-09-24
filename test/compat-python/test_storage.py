"""Cloud Storage through the official google-cloud-storage client.

Configured by STORAGE_EMULATOR_HOST alone, which the Python client needs with
a scheme: the Go client accepts a bare host, Python does not, and
`cloudburrow env` exports the form both accept.
"""

import io

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
