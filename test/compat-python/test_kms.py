"""Cloud KMS through the official google-cloud-kms client.

No Python client reads an emulator variable for Cloud KMS, so the channel is
built explicitly: plaintext gRPC to CLOUDBURROW_KMS_ENDPOINT, the recipe
docs/credentials.md gives for Cloud Tasks and Secret Manager. Skipped where
the instance does not run KMS.
"""

import google_crc32c
import grpc
import pytest
from google.api_core import exceptions
from google.cloud import kms
from google.cloud.kms_v1.services.key_management_service.transports import KeyManagementServiceGrpcTransport

from conftest import channel_target, require


def crc32c(data):
    # What the KMS samples use; already in the lock through the storage client.
    return google_crc32c.value(data)


def test_key_ring_key_encrypt_and_decrypt(project, suffix):
    endpoint = require("CLOUDBURROW_KMS_ENDPOINT")
    channel = grpc.insecure_channel(channel_target(endpoint))
    client = kms.KeyManagementServiceClient(transport=KeyManagementServiceGrpcTransport(channel=channel))

    location = f"projects/{project}/locations/global"
    # Key rings cannot be deleted, in Google Cloud or here: the ID is unique
    # per run so a rerun against one instance does not collide.
    ring = client.create_key_ring(parent=location, key_ring_id=f"py-{suffix}", key_ring={})
    assert client.get_key_ring(name=ring.name).name == ring.name
    assert ring.name in [r.name for r in client.list_key_rings(parent=location)]

    key = client.create_crypto_key(parent=ring.name, crypto_key_id="py-key", crypto_key={
        "purpose": kms.CryptoKey.CryptoKeyPurpose.ENCRYPT_DECRYPT,
        "version_template": {"algorithm": kms.CryptoKeyVersion.CryptoKeyVersionAlgorithm.GOOGLE_SYMMETRIC_ENCRYPTION},
    })
    assert key.primary.state == kms.CryptoKeyVersion.CryptoKeyVersionState.ENABLED

    plaintext = b"hello from python"
    aad = b"context"
    enc = client.encrypt(request={"name": key.name, "plaintext": plaintext, "plaintext_crc32c": crc32c(plaintext),
                                  "additional_authenticated_data": aad})
    # The integrity fields the KMS samples tell every caller to check.
    assert enc.verified_plaintext_crc32c
    assert enc.ciphertext_crc32c == crc32c(enc.ciphertext)
    assert enc.name.startswith(key.name + "/cryptoKeyVersions/")
    assert enc.ciphertext != plaintext

    dec = client.decrypt(request={"name": key.name, "ciphertext": enc.ciphertext,
                                  "ciphertext_crc32c": crc32c(enc.ciphertext), "additional_authenticated_data": aad})
    assert dec.plaintext == plaintext
    assert dec.plaintext_crc32c == crc32c(plaintext)

    # The wrong additional data does not decrypt.
    with pytest.raises(exceptions.GoogleAPICallError):
        client.decrypt(request={"name": key.name, "ciphertext": enc.ciphertext, "additional_authenticated_data": b"other"})
